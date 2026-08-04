package block_stm

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	storetypes "cosmossdk.io/store/types"
	"github.com/test-go/testify/require"
)

func TestMaxSpeculativeInflightPreservesSequentialState(t *testing.T) {
	stores := map[storetypes.StoreKey]int{StoreKeyAuth: 0, StoreKeyBank: 1}
	testCases := []struct {
		name             string
		limit            int
		wantApplied      bool
		wantTelemetry    bool
		wantEffectiveMax uint64
	}{
		{name: "one", limit: 1, wantApplied: true, wantTelemetry: true, wantEffectiveMax: 1},
		{name: "two", limit: 2, wantApplied: true, wantTelemetry: true, wantEffectiveMax: 2},
		{name: "eight", limit: 8, wantApplied: true, wantTelemetry: true, wantEffectiveMax: 8},
		{name: "default-W", limit: 0, wantEffectiveMax: 64},
		{name: "explicit-W", limit: 64, wantEffectiveMax: 64},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			block := testBlock(64, 12)
			storage := NewMultiMemDB(stores)
			stats, err := ExecuteBlockWithMaxSpeculativeInflight(
				context.Background(), block.Size(), stores, storage, 8, testCase.limit, block.ExecuteTx,
			)
			require.NoError(t, err)

			sequential := NewMultiMemDB(stores)
			runSequential(sequential, block)
			for store := range stores {
				require.True(t, StoreEqual(sequential.GetKVStore(store), storage.GetKVStore(store)))
			}
			require.Equal(t, testCase.wantEffectiveMax, stats.EffectiveLimit)
			require.Equal(t, testCase.wantApplied, stats.LimitApplied)
			require.Equal(t, testCase.wantTelemetry, stats.TelemetryAvailable)
			if stats.TelemetryAvailable {
				if stats.PeakInflight == 0 || stats.PeakInflight > stats.EffectiveLimit {
					t.Fatalf("invalid peak inflight telemetry: %#v", stats)
				}
			}
		})
	}
}

func TestAdmissionLimitDoesNotExposeNextTransactionBeforeStableFrontier(t *testing.T) {
	const blockSize = 4
	started := make([]chan struct{}, blockSize)
	release := make([]chan struct{}, blockSize)
	for index := 0; index < blockSize; index++ {
		started[index] = make(chan struct{})
		release[index] = make(chan struct{})
	}

	var once [blockSize]sync.Once
	execute := func(transaction TxnIndex, _ MultiStore) {
		once[transaction].Do(func() { close(started[transaction]) })
		<-release[transaction]
	}
	storage := NewMultiMemDB(map[storetypes.StoreKey]int{})
	type outcome struct {
		stats SpeculationStats
		err   error
	}
	completed := make(chan outcome, 1)
	go func() {
		stats, err := ExecuteBlockWithMaxSpeculativeInflight(
			context.Background(), blockSize, map[storetypes.StoreKey]int{}, storage, 4, 2, execute,
		)
		completed <- outcome{stats: stats, err: err}
	}()

	waitClosed(t, started[0])
	waitClosed(t, started[1])
	select {
	case <-started[2]:
		t.Fatal("transaction 2 was exposed before the first two transactions reached the stable frontier")
	case <-time.After(20 * time.Millisecond):
	}
	close(release[0])
	close(release[1])
	waitClosed(t, started[2])
	waitClosed(t, started[3])
	close(release[2])
	close(release[3])

	result := <-completed
	require.NoError(t, result.err)
	require.Equal(t, uint64(2), result.stats.EffectiveLimit)
	require.Equal(t, uint64(2), result.stats.PeakInflight)
	if result.stats.AdmissionStallEvents == 0 {
		t.Fatalf("admission stalls were not recorded: %#v", result.stats)
	}
}

func TestNewWritePathInvalidatesPreviouslyPassedSuffix(t *testing.T) {
	scheduler := newAdmissionScheduler(4, 2)
	scheduler.base.execution_idx.Store(2)
	scheduler.base.validation_idx.Store(2)
	scheduler.base.num_active_tasks.Store(1)
	if _, ok := scheduler.base.txn_status[0].TrySetExecuting(); !ok {
		t.Fatal("failed to prepare executing predecessor")
	}
	scheduler.passed[1] = true
	previousEpoch := scheduler.epoch[1]

	scheduler.finishExecution(TxnVersion{Index: 0}, true)
	if scheduler.passed[1] || scheduler.epoch[1] == previousEpoch {
		t.Fatalf("new predecessor write path did not invalidate suffix: passed=%v epoch=%d", scheduler.passed, scheduler.epoch[1])
	}
	if scheduler.base.validation_idx.Load() != 0 {
		t.Fatalf("base validation frontier was not rewound: %d", scheduler.base.validation_idx.Load())
	}
}

func TestMaxSpeculativeInflightRejectsInvalidLimit(t *testing.T) {
	_, err := ExecuteBlockWithMaxSpeculativeInflight(
		context.Background(), 1, map[storetypes.StoreKey]int{}, NewMultiMemDB(map[storetypes.StoreKey]int{}), 1, -1,
		func(TxnIndex, MultiStore) {},
	)
	if err == nil || errors.Is(err, context.Canceled) {
		t.Fatalf("invalid limit was accepted: %v", err)
	}
}

func TestStaticEstimateMissesAndFalsePositivesRemainSafe(t *testing.T) {
	stores := map[storetypes.StoreKey]int{StoreKeyAuth: 0, StoreKeyBank: 1}
	block := testBlock(64, 4)
	// The estimates are deliberately incomplete and include a false-positive
	// location. They are acceleration hints, not a correctness dependency.
	estimates := make([]MultiLocations, block.Size())
	estimates[0] = MultiLocations{0: Locations{Key("not-accessed")}}

	sequential := NewMultiMemDB(stores)
	runSequential(sequential, block)
	for _, limit := range []int{0, 3} {
		storage := NewMultiMemDB(stores)
		_, err := ExecuteBlockWithMaxSpeculativeInflightAndEstimates(
			context.Background(), block.Size(), stores, storage, 8, limit, estimates, block.ExecuteTx,
		)
		require.NoError(t, err)
		for store := range stores {
			require.True(t, StoreEqual(sequential.GetKVStore(store), storage.GetKVStore(store)))
		}
	}
}

func waitClosed(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for transaction execution")
	}
}
