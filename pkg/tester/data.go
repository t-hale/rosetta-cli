// Copyright 2020 Coinbase, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package tester

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/coinbase/rosetta-cli/configuration"
	"github.com/coinbase/rosetta-cli/pkg/logger"
	"github.com/coinbase/rosetta-cli/pkg/processor"
	"github.com/coinbase/rosetta-cli/pkg/statefulsyncer"
	"github.com/coinbase/rosetta-cli/pkg/storage"
	"github.com/coinbase/rosetta-cli/pkg/utils"

	"github.com/coinbase/rosetta-sdk-go/fetcher"
	"github.com/coinbase/rosetta-sdk-go/reconciler"
	"github.com/coinbase/rosetta-sdk-go/types"
	"github.com/fatih/color"
	"golang.org/x/sync/errgroup"
)

const (
	// dataCmdName is used as the prefix on the data directory
	// for all data saved using this command.
	dataCmdName = "check-data"

	// InactiveFailureLookbackWindow is the size of each window to check
	// for missing ops. If a block with missing ops is not found in this
	// window, another window is created with the preceding
	// InactiveFailureLookbackWindow blocks (this process continues
	// until the client halts the search or the block is found).
	InactiveFailureLookbackWindow = 250

	// PeriodicLoggingFrequency is the frequency that stats are printed
	// to the terminal.
	//
	// TODO: make configurable
	PeriodicLoggingFrequency = 10 * time.Second

	// EndAtTipCheckInterval is the frequency that EndAtTip condition
	// is evaludated
	//
	// TODO: make configurable
	EndAtTipCheckInterval = 10 * time.Second
)

// DataTester coordinates the `check:data` test.
type DataTester struct {
	network           *types.NetworkIdentifier
	database          storage.Database
	config            *configuration.Configuration
	syncer            *statefulsyncer.StatefulSyncer
	reconciler        *reconciler.Reconciler
	logger            *logger.Logger
	balanceStorage    *storage.BalanceStorage
	blockStorage      *storage.BlockStorage
	counterStorage    *storage.CounterStorage
	reconcilerHandler *processor.ReconcilerHandler
	fetcher           *fetcher.Fetcher
	signalReceived    *bool
	genesisBlock      *types.BlockIdentifier
	cancel            context.CancelFunc

	endCondition       configuration.CheckDataEndCondition
	endConditionDetail string
}

func shouldReconcile(config *configuration.Configuration) bool {
	if config.Data.BalanceTrackingDisabled {
		return false
	}

	if config.Data.ReconciliationDisabled {
		return false
	}

	return true
}

// loadAccounts is a utility function to parse the []*reconciler.AccountCurrency
// in a file.
func loadAccounts(filePath string) ([]*reconciler.AccountCurrency, error) {
	if len(filePath) == 0 {
		return []*reconciler.AccountCurrency{}, nil
	}

	accounts := []*reconciler.AccountCurrency{}
	if err := utils.LoadAndParse(filePath, &accounts); err != nil {
		return nil, fmt.Errorf("%w: unable to open account file", err)
	}

	log.Printf(
		"Found %d accounts at %s: %s\n",
		len(accounts),
		filePath,
		types.PrettyPrintStruct(accounts),
	)

	return accounts, nil
}

// CloseDatabase closes the database used by DataTester.
func (t *DataTester) CloseDatabase(ctx context.Context) {
	if err := t.database.Close(ctx); err != nil {
		log.Fatalf("%s: error closing database", err.Error())
	}
}

// InitializeData returns a new *DataTester.
func InitializeData(
	ctx context.Context,
	config *configuration.Configuration,
	network *types.NetworkIdentifier,
	fetcher *fetcher.Fetcher,
	cancel context.CancelFunc,
	genesisBlock *types.BlockIdentifier,
	interestingAccount *reconciler.AccountCurrency,
	signalReceived *bool,
) *DataTester {
	dataPath, err := utils.CreateCommandPath(config.DataDirectory, dataCmdName, network)
	if err != nil {
		log.Fatalf("%s: cannot create command path", err.Error())
	}

	localStore, err := storage.NewBadgerStorage(ctx, dataPath, config.DisableMemoryLimit)
	if err != nil {
		log.Fatalf("%s: unable to initialize database", err.Error())
	}

	exemptAccounts, err := loadAccounts(config.Data.ExemptAccounts)
	if err != nil {
		log.Fatalf("%s: unable to load exempt accounts", err.Error())
	}

	interestingAccounts, err := loadAccounts(config.Data.InterestingAccounts)
	if err != nil {
		log.Fatalf("%s: unable to load interesting accounts", err.Error())
	}

	counterStorage := storage.NewCounterStorage(localStore)
	blockStorage := storage.NewBlockStorage(localStore)
	balanceStorage := storage.NewBalanceStorage(localStore)

	loggerBalanceStorage := balanceStorage
	if !shouldReconcile(config) {
		loggerBalanceStorage = nil
	}

	logger := logger.NewLogger(
		counterStorage,
		loggerBalanceStorage,
		dataPath,
		config.Data.LogBlocks,
		config.Data.LogTransactions,
		config.Data.LogBalanceChanges,
		config.Data.LogReconciliations,
	)

	reconcilerHelper := processor.NewReconcilerHelper(
		network,
		fetcher,
		blockStorage,
		balanceStorage,
	)

	reconcilerHandler := processor.NewReconcilerHandler(
		logger,
		balanceStorage,
		!config.Data.IgnoreReconciliationError,
	)

	// Get all previously seen accounts
	seenAccounts, err := balanceStorage.GetAllAccountCurrency(ctx)
	if err != nil {
		log.Fatalf("%s: unable to get previously seen accounts", err.Error())
	}

	r := reconciler.New(
		reconcilerHelper,
		reconcilerHandler,
		reconciler.WithActiveConcurrency(int(config.Data.ActiveReconciliationConcurrency)),
		reconciler.WithInactiveConcurrency(int(config.Data.InactiveReconciliationConcurrency)),
		reconciler.WithLookupBalanceByBlock(!config.Data.HistoricalBalanceDisabled),
		reconciler.WithInterestingAccounts(interestingAccounts),
		reconciler.WithSeenAccounts(seenAccounts),
		reconciler.WithDebugLogging(config.Data.LogReconciliations),
		reconciler.WithInactiveFrequency(int64(config.Data.InactiveReconciliationFrequency)),
	)

	blockWorkers := []storage.BlockWorker{}
	if !config.Data.BalanceTrackingDisabled {
		balanceStorageHelper := processor.NewBalanceStorageHelper(
			network,
			fetcher,
			!config.Data.HistoricalBalanceDisabled,
			exemptAccounts,
			false,
		)

		balanceStorageHandler := processor.NewBalanceStorageHandler(
			logger,
			r,
			shouldReconcile(config),
			interestingAccount,
		)

		balanceStorage.Initialize(balanceStorageHelper, balanceStorageHandler)

		// Bootstrap balances if provided
		if len(config.Data.BootstrapBalances) > 0 {
			_, err := blockStorage.GetHeadBlockIdentifier(ctx)
			if err == storage.ErrHeadBlockNotFound {
				err = balanceStorage.BootstrapBalances(
					ctx,
					config.Data.BootstrapBalances,
					genesisBlock,
				)
				if err != nil {
					log.Fatalf("%s: unable to bootstrap balances", err.Error())
				}
			} else {
				log.Println("Skipping balance bootstrapping because already started syncing")
			}
		}

		blockWorkers = append(blockWorkers, balanceStorage)
	}

	if !config.Data.CoinTrackingDisabled {
		coinStorageHelper := processor.NewCoinStorageHelper(blockStorage)
		coinStorage := storage.NewCoinStorage(localStore, coinStorageHelper, fetcher.Asserter)

		blockWorkers = append(blockWorkers, coinStorage)
	}

	syncer := statefulsyncer.New(
		ctx,
		network,
		fetcher,
		blockStorage,
		counterStorage,
		logger,
		cancel,
		blockWorkers,
		config.SyncConcurrency,
	)

	return &DataTester{
		network:           network,
		database:          localStore,
		config:            config,
		syncer:            syncer,
		cancel:            cancel,
		reconciler:        r,
		logger:            logger,
		balanceStorage:    balanceStorage,
		blockStorage:      blockStorage,
		counterStorage:    counterStorage,
		reconcilerHandler: reconcilerHandler,
		fetcher:           fetcher,
		signalReceived:    signalReceived,
		genesisBlock:      genesisBlock,
	}
}

// StartSyncing syncs from startIndex to endIndex.
// If startIndex is -1, it will start from the last
// saved block. If endIndex is -1, it will sync
// continuously (or until an error).
func (t *DataTester) StartSyncing(
	ctx context.Context,
) error {
	startIndex := int64(-1)
	if t.config.Data.StartIndex != nil {
		startIndex = *t.config.Data.StartIndex
	}

	endIndex := int64(-1)
	if t.config.Data.EndConditions != nil && t.config.Data.EndConditions.Index != nil {
		endIndex = *t.config.Data.EndConditions.Index
	}

	return t.syncer.Sync(ctx, startIndex, endIndex)
}

// StartReconciler starts the reconciler if
// reconciliation is enabled.
func (t *DataTester) StartReconciler(
	ctx context.Context,
) error {
	if !shouldReconcile(t.config) {
		return nil
	}

	return t.reconciler.Reconcile(ctx)
}

// StartPeriodicLogger prints out periodic
// stats about a run of `check:data`.
func (t *DataTester) StartPeriodicLogger(
	ctx context.Context,
) error {
	tc := time.NewTicker(PeriodicLoggingFrequency)
	defer tc.Stop()

	for {
		select {
		case <-ctx.Done():
			// Print stats one last time before exiting
			_ = t.logger.LogDataStats(ctx)

			return ctx.Err()
		case <-tc.C:
			_ = t.logger.LogDataStats(ctx)
		}
	}
}

// EndAtTipLoop runs a loop that evaluates end condition EndAtTip
func (t *DataTester) EndAtTipLoop(
	ctx context.Context,
	minReconciliationCoverage float64,
) {
	tc := time.NewTicker(EndAtTipCheckInterval)
	defer tc.Stop()

	firstTipIndex := int64(-1)

	for {
		select {
		case <-ctx.Done():
			return

		case <-tc.C:
			atTip, blockIdentifier, err := t.blockStorage.AtTip(ctx, t.config.TipDelay)
			if err != nil {
				log.Printf(
					"%s: unable to evaluate if syncer is at tip",
					err.Error(),
				)
				continue
			}

			// If we fall behind tip, we must reset the firstTipIndex.
			if !atTip {
				firstTipIndex = int64(-1)
				continue
			}

			// If minReconciliationCoverage is less than 0,
			// we should just stop at tip.
			if minReconciliationCoverage < 0 {
				t.endCondition = configuration.TipEndCondition
				t.endConditionDetail = fmt.Sprintf(
					"Tip: %d",
					blockIdentifier.Index,
				)
				t.cancel()
				return
			}

			// Once at tip, we want to consider
			// coverage. It is not feasible that we could
			// get high reconciliation coverage at the tip
			// block, so we take the range from when first
			// at tip to the current block.
			if firstTipIndex < 0 {
				firstTipIndex = blockIdentifier.Index
			}

			coverage, err := t.balanceStorage.ReconciliationCoverage(ctx, firstTipIndex)
			if err != nil {
				log.Printf(
					"%s: unable to get reconciliations coverage",
					err.Error(),
				)
				continue
			}

			if coverage >= minReconciliationCoverage {
				t.endCondition = configuration.ReconciliationCoverageEndCondition
				t.endConditionDetail = fmt.Sprintf(
					"Coverage: %f%%",
					coverage*utils.OneHundred,
				)
				t.cancel()
				return
			}
		}
	}
}

// EndDurationLoop runs a loop that evaluates end condition EndDuration.
func (t *DataTester) EndDurationLoop(
	ctx context.Context,
	duration time.Duration,
) {
	timer := time.NewTimer(duration)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return

		case <-timer.C:
			t.endCondition = configuration.DurationEndCondition
			t.endConditionDetail = fmt.Sprintf(
				"Seconds: %d",
				int(duration.Seconds()),
			)
			t.cancel()
			return
		}
	}
}

// WatchEndConditions starts go routines to watch the end conditions
func (t *DataTester) WatchEndConditions(
	ctx context.Context,
) error {
	endConds := t.config.Data.EndConditions
	if endConds == nil {
		return nil
	}

	if endConds.Tip != nil && *endConds.Tip {
		// runs a go routine that ends when reaching tip
		go t.EndAtTipLoop(ctx, -1)
	}

	if endConds.Duration != nil && *endConds.Duration != 0 {
		// runs a go routine that ends after a duration
		go t.EndDurationLoop(ctx, time.Duration(*endConds.Duration)*time.Second)
	}

	if endConds.ReconciliationCoverage != nil {
		go t.EndAtTipLoop(ctx, *endConds.ReconciliationCoverage)
	}

	return nil
}

// HandleErr is called when `check:data` returns an error.
// If historical balance lookups are enabled, HandleErr will attempt to
// automatically find any missing balance-changing operations.
func (t *DataTester) HandleErr(ctx context.Context, err error, sigListeners []context.CancelFunc) {
	if *t.signalReceived {
		color.Red("Check halted")
		os.Exit(1)
		return
	}

	if (err == nil || err == context.Canceled) && len(t.endCondition) == 0 && t.config.Data.EndConditions != nil &&
		t.config.Data.EndConditions.Index != nil { // occurs at syncer end
		t.endCondition = configuration.IndexEndCondition
		t.endConditionDetail = fmt.Sprintf(
			"Index: %d",
			*t.config.Data.EndConditions.Index,
		)
	}

	if len(t.endCondition) != 0 {
		color.Green(fmt.Sprintf("Check succeeded: %s [%s]", t.endCondition, t.endConditionDetail))
		Exit(t.config, t.counterStorage, t.balanceStorage, nil, 0, t.endCondition, t.endConditionDetail)
	}

	color.Red("Check failed!")
	if t.reconcilerHandler.InactiveFailure == nil {
		Exit(t.config, t.counterStorage, t.balanceStorage, err, 1, "", "")
	}

	if t.config.Data.HistoricalBalanceDisabled {
		color.Yellow(
			"Can't find the block missing operations automatically, please enable --lookup-balance-by-block",
		)
		Exit(t.config, t.counterStorage, t.balanceStorage, err, 1, "", "")
	}

	if t.config.Data.InactiveDiscrepencySearchDisabled {
		color.Yellow("Search for inactive reconciliation discrepency is disabled")
		Exit(t.config, t.counterStorage, t.balanceStorage, err, 1, "", "")
	}

	t.FindMissingOps(ctx, err, sigListeners)
}

// FindMissingOps logs the types.BlockIdentifier of a block
// that is missing balance-changing operations for a
// *reconciler.AccountCurrency.
func (t *DataTester) FindMissingOps(
	ctx context.Context,
	originalErr error,
	sigListeners []context.CancelFunc,
) {
	color.Cyan("Searching for block with missing operations...hold tight")
	badBlock, err := t.recursiveOpSearch(
		ctx,
		&sigListeners,
		t.reconcilerHandler.InactiveFailure,
		t.reconcilerHandler.InactiveFailureBlock.Index-InactiveFailureLookbackWindow,
		t.reconcilerHandler.InactiveFailureBlock.Index,
	)
	if err != nil {
		color.Yellow("%s: could not find block with missing ops", err.Error())
		Exit(t.config, t.counterStorage, t.balanceStorage, originalErr, 1, "", "")
	}

	color.Yellow(
		"Missing ops for %s in block %d:%s",
		types.AccountString(t.reconcilerHandler.InactiveFailure.Account),
		badBlock.Index,
		badBlock.Hash,
	)
	Exit(t.config, t.counterStorage, t.balanceStorage, originalErr, 1, "", "")
}

func (t *DataTester) recursiveOpSearch(
	ctx context.Context,
	sigListeners *[]context.CancelFunc,
	accountCurrency *reconciler.AccountCurrency,
	startIndex int64,
	endIndex int64,
) (*types.BlockIdentifier, error) {
	// To cancel all execution, need to call multiple cancel functions.
	ctx, cancel := context.WithCancel(ctx)
	*sigListeners = append(*sigListeners, cancel)

	// Always use a temporary directory to find missing ops
	tmpDir, err := utils.CreateTempDir()
	if err != nil {
		return nil, fmt.Errorf("%w: unable to create temporary directory", err)
	}
	defer utils.RemoveTempDir(tmpDir)

	localStore, err := storage.NewBadgerStorage(ctx, tmpDir, t.config.DisableMemoryLimit)
	if err != nil {
		return nil, fmt.Errorf("%w: unable to initialize database", err)
	}

	counterStorage := storage.NewCounterStorage(localStore)
	blockStorage := storage.NewBlockStorage(localStore)
	balanceStorage := storage.NewBalanceStorage(localStore)

	logger := logger.NewLogger(
		counterStorage,
		nil,
		tmpDir,
		false,
		false,
		false,
		false,
	)

	reconcilerHelper := processor.NewReconcilerHelper(
		t.network,
		t.fetcher,
		blockStorage,
		balanceStorage,
	)

	reconcilerHandler := processor.NewReconcilerHandler(
		logger,
		balanceStorage,
		true, // halt on reconciliation error
	)

	r := reconciler.New(
		reconcilerHelper,
		reconcilerHandler,

		// When using concurrency > 1, we could start looking up balance changes
		// on multiple blocks at once. This can cause us to return the wrong block
		// that is missing operations.
		reconciler.WithActiveConcurrency(1),

		// Do not do any inactive lookups when looking for the block with missing
		// operations.
		reconciler.WithInactiveConcurrency(0),
		reconciler.WithLookupBalanceByBlock(!t.config.Data.HistoricalBalanceDisabled),
		reconciler.WithInterestingAccounts([]*reconciler.AccountCurrency{accountCurrency}),
	)

	balanceStorageHelper := processor.NewBalanceStorageHelper(
		t.network,
		t.fetcher,
		!t.config.Data.HistoricalBalanceDisabled,
		nil,
		false,
	)

	balanceStorageHandler := processor.NewBalanceStorageHandler(
		logger,
		r,
		true,
		accountCurrency,
	)

	balanceStorage.Initialize(balanceStorageHelper, balanceStorageHandler)

	syncer := statefulsyncer.New(
		ctx,
		t.network,
		t.fetcher,
		blockStorage,
		counterStorage,
		logger,
		cancel,
		[]storage.BlockWorker{balanceStorage},
		t.config.SyncConcurrency,
	)

	g, ctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		return r.Reconcile(ctx)
	})

	g.Go(func() error {
		return syncer.Sync(
			ctx,
			startIndex,
			endIndex,
		)
	})

	err = g.Wait()

	// Close database before starting another search, otherwise we will
	// have n databases open when we find the offending block.
	if storageErr := localStore.Close(ctx); storageErr != nil {
		return nil, fmt.Errorf("%w: unable to close database", storageErr)
	}

	if *t.signalReceived {
		return nil, errors.New("Search for block with missing ops halted")
	}

	if err == nil || err == context.Canceled {
		newStart := startIndex - InactiveFailureLookbackWindow
		if newStart < t.genesisBlock.Index {
			newStart = t.genesisBlock.Index
		}

		newEnd := endIndex - InactiveFailureLookbackWindow
		if newEnd <= newStart {
			return nil, fmt.Errorf(
				"Next window to check has start index %d <= end index %d",
				newStart,
				newEnd,
			)
		}

		color.Cyan(
			"Unable to find missing ops in block range %d-%d, now searching %d-%d",
			startIndex, endIndex,
			newStart,
			newEnd,
		)

		return t.recursiveOpSearch(
			// We need to use new context for each invocation because the syncer
			// cancels the provided context when it reaches the end of a syncing
			// window.
			context.Background(),
			sigListeners,
			accountCurrency,
			startIndex-InactiveFailureLookbackWindow,
			endIndex-InactiveFailureLookbackWindow,
		)
	}

	if reconcilerHandler.ActiveFailureBlock == nil {
		return nil, errors.New("unable to find missing ops")
	}

	return reconcilerHandler.ActiveFailureBlock, nil
}
