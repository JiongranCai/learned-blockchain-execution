package block_stm

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	storetypes "cosmossdk.io/store/types"
	"github.com/test-go/testify/require"
)

// policyMatrix enumerates the additive kernel policies. Iterating blocks are
// excluded for the non-default estimate policies because range reads keep
// frozen suspend-in-place semantics by design.
func policyMatrix() []struct {
	name    string
	policy  KernelPolicy
	iterate bool
} {
	return []struct {
		name    string
		policy  KernelPolicy
		iterate bool
	}{
		{"frozen-default", KernelPolicy{}, true},
		{"park-idle", KernelPolicy{IdleWait: IdleWaitPark}, true},
		{"abort-reschedule", KernelPolicy{EstimateRead: EstimateReadAbortReschedule}, false},
		{"abort-reschedule-park", KernelPolicy{EstimateRead: EstimateReadAbortReschedule, IdleWait: IdleWaitPark}, false},
		{"suspend-yield-worker", KernelPolicy{EstimateRead: EstimateReadSuspendYieldWorker}, false},
	}
}

func TestKernelPolicyMatchesSequential(t *testing.T) {
	stores := map[storetypes.StoreKey]int{StoreKeyAuth: 0, StoreKeyBank: 1}
	blocks := []struct {
		name    string
		build   func() *MockBlock
		iterate bool
	}{
		{"random-100-80", func() *MockBlock { return testBlock(100, 80) }, false},
		{"random-100-3", func() *MockBlock { return testBlock(100, 3) }, false},
		{"no-conflict-100", func() *MockBlock { return noConflictBlock(100) }, false},
		{"worst-case-100", func() *MockBlock { return worstCaseBlock(100) }, false},
		{"iterate-100-10", func() *MockBlock { return iterateBlock(100, 10) }, true},
	}

	for _, entry := range policyMatrix() {
		for _, blockCase := range blocks {
			if blockCase.iterate && !entry.iterate {
				continue
			}
			for _, executors := range []int{1, 4, 8} {
				name := entry.name + "/" + blockCase.name + "/executors-" + strconv.Itoa(executors)
				t.Run(name, func(t *testing.T) {
					block := blockCase.build()
					storage := NewMultiMemDB(stores)
					_, stats, err := ExecuteBlockWithKernelPolicy(
						context.Background(), block.Size(), stores, storage,
						executors, 0, nil, entry.policy, block.ExecuteTx,
					)
					require.NoError(t, err)
					for _, result := range block.Results {
						require.NoError(t, result)
					}

					reference := NewMultiMemDB(stores)
					runSequential(reference, blockCase.build())
					for store := range stores {
						require.True(t, StoreEqual(reference.GetKVStore(store), storage.GetKVStore(store)),
							"parallel result differs from sequential reference")
					}
					require.Equal(t, entry.policy.IsFrozenDefault(), !stats.Applied)
				})
			}
		}
	}
}

func TestKernelPolicyReadyQueueDispatch(t *testing.T) {
	stores := map[storetypes.StoreKey]int{StoreKeyAuth: 0, StoreKeyBank: 1}
	size := 60

	gates := []struct {
		name  string
		build func(int) KernelPolicy
	}{
		{"predecessor-chain", func(n int) KernelPolicy {
			predecessors := make([][]int, n)
			for i := 1; i < n; i++ {
				predecessors[i] = []int{i - 1}
			}
			return KernelPolicy{Dispatch: DispatchReadyQueue, Predecessors: predecessors}
		}},
		{"predecessor-sparse", func(n int) KernelPolicy {
			predecessors := make([][]int, n)
			for i := 2; i < n; i++ {
				predecessors[i] = []int{i / 2}
			}
			return KernelPolicy{Dispatch: DispatchReadyQueue, Predecessors: predecessors}
		}},
		{"predecessor-none", func(n int) KernelPolicy {
			return KernelPolicy{Dispatch: DispatchReadyQueue, Predecessors: make([][]int, n)}
		}},
		{"barrier-frontier", func(n int) KernelPolicy {
			barriers := make([]int, n)
			for i := range barriers {
				barriers[i] = i - 1
			}
			return KernelPolicy{Dispatch: DispatchReadyQueue, Barriers: barriers}
		}},
		{"barrier-none", func(n int) KernelPolicy {
			barriers := make([]int, n)
			for i := range barriers {
				barriers[i] = -1
			}
			return KernelPolicy{Dispatch: DispatchReadyQueue, Barriers: barriers}
		}},
	}

	for _, gate := range gates {
		for _, estimate := range []EstimateReadPolicy{
			EstimateReadSuspendInPlace,
			EstimateReadAbortReschedule,
			EstimateReadSuspendYieldWorker,
		} {
			for _, setting := range []struct{ executors, limit int }{{1, 0}, {4, 0}, {8, 0}, {8, 1}, {8, 8}} {
				name := gate.name + "/" + string(estimate) + "/executors-" + strconv.Itoa(setting.executors) + "/L-" + strconv.Itoa(setting.limit)
				t.Run(name, func(t *testing.T) {
					policy := gate.build(size)
					policy.EstimateRead = estimate
					block := testBlock(size, 4)
					storage := NewMultiMemDB(stores)
					_, stats, err := ExecuteBlockWithKernelPolicy(
						context.Background(), size, stores, storage,
						setting.executors, setting.limit, nil, policy, block.ExecuteTx,
					)
					require.NoError(t, err)

					reference := NewMultiMemDB(stores)
					runSequential(reference, testBlock(size, 4))
					for store := range stores {
						require.True(t, StoreEqual(reference.GetKVStore(store), storage.GetKVStore(store)))
					}
					require.True(t, stats.Applied)
				})
			}
		}
	}
}

func TestKernelPolicyEstimateInjectionMatchesSequential(t *testing.T) {
	stores := map[storetypes.StoreKey]int{StoreKeyAuth: 0, StoreKeyBank: 1}
	size := 80
	// Pre-seeded estimates force ESTIMATE reads on the very first pass, which
	// is the path the estimate_read policy governs.
	estimates := make([]MultiLocations, size)
	for i := range estimates {
		estimates[i] = MultiLocations{0: Locations{Key(accountName(int64(i % 3)))}}
	}

	for _, entry := range policyMatrix() {
		t.Run(entry.name, func(t *testing.T) {
			block := testBlock(size, 3)
			storage := NewMultiMemDB(stores)
			_, _, err := ExecuteBlockWithKernelPolicy(
				context.Background(), size, stores, storage,
				8, 0, estimates, entry.policy, block.ExecuteTx,
			)
			require.NoError(t, err)

			reference := NewMultiMemDB(stores)
			runSequential(reference, testBlock(size, 3))
			for store := range stores {
				require.True(t, StoreEqual(reference.GetKVStore(store), storage.GetKVStore(store)))
			}
		})
	}
}

func TestKernelPolicyValidation(t *testing.T) {
	_, err := KernelPolicy{EstimateRead: "nope"}.Normalize()
	require.True(t, errors.Is(err, ErrKernelPolicyUnknown), "expected %v, got %v", ErrKernelPolicyUnknown, err)

	_, err = KernelPolicy{IdleWait: "nope"}.Normalize()
	require.True(t, errors.Is(err, ErrKernelPolicyUnknown), "expected %v, got %v", ErrKernelPolicyUnknown, err)

	_, err = KernelPolicy{Dispatch: "nope"}.Normalize()
	require.True(t, errors.Is(err, ErrKernelPolicyUnknown), "expected %v, got %v", ErrKernelPolicyUnknown, err)

	_, err = KernelPolicy{Dispatch: DispatchReadyQueue}.Normalize()
	require.True(t, errors.Is(err, ErrKernelPolicyGate), "expected %v, got %v", ErrKernelPolicyGate, err)

	_, err = KernelPolicy{Predecessors: make([][]int, 2)}.Normalize()
	require.True(t, errors.Is(err, ErrKernelPolicyGate), "expected %v, got %v", ErrKernelPolicyGate, err)

	// suspend_yield_worker forces parked idle waiting.
	normalized, err := KernelPolicy{EstimateRead: EstimateReadSuspendYieldWorker}.Normalize()
	require.NoError(t, err)
	require.Equal(t, IdleWaitPark, normalized.IdleWait)

	require.True(t, KernelPolicy{}.IsFrozenDefault())
	require.False(t, KernelPolicy{IdleWait: IdleWaitPark}.IsFrozenDefault())

	// Forward edges only: a gate must never be able to reorder preset order.
	stores := map[storetypes.StoreKey]int{StoreKeyAuth: 0, StoreKeyBank: 1}
	predecessors := [][]int{{}, {1}}
	_, _, err = ExecuteBlockWithKernelPolicy(
		context.Background(), 2, stores, NewMultiMemDB(stores), 2, 0, nil,
		KernelPolicy{Dispatch: DispatchReadyQueue, Predecessors: predecessors},
		func(TxnIndex, MultiStore) {},
	)
	require.True(t, errors.Is(err, ErrKernelPolicyGate), "expected %v, got %v", ErrKernelPolicyGate, err)

	// The policy kernel applies the finite speculation window.
	window, kernel, err := ExecuteBlockWithKernelPolicy(
		context.Background(), 8, stores, NewMultiMemDB(stores), 2, 2, nil,
		KernelPolicy{IdleWait: IdleWaitPark},
		func(TxnIndex, MultiStore) {},
	)
	require.NoError(t, err)
	require.True(t, kernel.Applied)
	require.True(t, window.LimitApplied)
	require.Equal(t, uint64(2), window.EffectiveLimit)
	require.True(t, window.PeakInflight <= 2)
}

// TestKernelPolicyArgumentValidationIsPathIndependent pins the fix for a
// public API inconsistency: the policy branch used to accept a negative
// speculation window that the frozen branch rejected.
func TestKernelPolicyArgumentValidationIsPathIndependent(t *testing.T) {
	stores := map[storetypes.StoreKey]int{StoreKeyAuth: 0, StoreKeyBank: 1}
	for _, entry := range policyMatrix() {
		t.Run(entry.name, func(t *testing.T) {
			_, _, err := ExecuteBlockWithKernelPolicy(
				context.Background(), 4, stores, NewMultiMemDB(stores), 2, -1, nil,
				entry.policy, func(TxnIndex, MultiStore) {},
			)
			require.Error(t, err, "a negative speculation window must be rejected on every path")

			_, _, err = ExecuteBlockWithKernelPolicy(
				context.Background(), 4, stores, NewMultiMemDB(stores), -1, 0, nil,
				entry.policy, func(TxnIndex, MultiStore) {},
			)
			require.Error(t, err, "a negative executor count must be rejected on every path")

			_, _, err = ExecuteBlockWithKernelPolicy(
				context.Background(), 1, stores, NewMultiMemDB(stores), 2, 0,
				make([]MultiLocations, 4), entry.policy, func(TxnIndex, MultiStore) {},
			)
			require.Error(t, err, "more estimates than transactions must be rejected on every path")
		})
	}
}

// TestKernelPolicyYieldRequiresPark pins that an explicit conflicting value is
// refused rather than silently rewritten, while an omitted one still resolves.
func TestKernelPolicyYieldRequiresPark(t *testing.T) {
	_, err := KernelPolicy{
		EstimateRead: EstimateReadSuspendYieldWorker,
		IdleWait:     IdleWaitGosched,
	}.Normalize()
	require.True(t, errors.Is(err, ErrKernelPolicyGate), "expected %v, got %v", ErrKernelPolicyGate, err)

	normalized, err := KernelPolicy{EstimateRead: EstimateReadSuspendYieldWorker}.Normalize()
	require.NoError(t, err)
	require.Equal(t, IdleWaitPark, normalized.IdleWait)
}

// TestIdleWaitDoesNotSleepWhenWorkExists pins the completion tail and the
// scan-progress checks. Parking is counted, so the assertions are exact rather
// than timing based.
func TestIdleWaitDoesNotSleepWhenWorkExists(t *testing.T) {
	policy := KernelPolicy{IdleWait: IdleWaitPark}

	// The block is finished: both scans are exhausted and there is nothing to
	// re-dispatch, so only the completion check can keep the worker awake.
	finished := newPolicyScheduler(4, policy)
	finished.base.execution_idx.Store(4)
	finished.base.validation_idx.Store(4)
	finished.base.done_marker.Store(true)
	finished.idleWait(context.Background())
	require.Zero(t, finished.idleParks.Load(), "a finished block must never be slept on")

	scanning := newPolicyScheduler(4, policy)
	scanning.idleWait(context.Background())
	require.Zero(t, scanning.idleParks.Load(), "an unfinished index scan must not be slept on")

	queued := newPolicyScheduler(4, policy)
	queued.base.execution_idx.Store(4)
	queued.base.validation_idx.Store(4)
	queued.ready = append(queued.ready, 0)
	queued.idleWait(context.Background())
	require.Zero(t, queued.idleParks.Load(), "a pending re-dispatch must not be slept on")
}

// TestIdleWaitWakesOnNotification pins that no wake is lost between the
// progress check and the park. The safety interval is raised so a wake-up can
// only come from the notification itself.
func TestIdleWaitWakesOnNotification(t *testing.T) {
	original := idleParkSafetyInterval
	idleParkSafetyInterval = time.Hour
	defer func() { idleParkSafetyInterval = original }()

	scheduler := newPolicyScheduler(4, KernelPolicy{IdleWait: IdleWaitPark})
	scheduler.base.execution_idx.Store(4)
	scheduler.base.validation_idx.Store(4)

	returned := make(chan struct{})
	go func() {
		scheduler.idleWait(context.Background())
		close(returned)
	}()
	// Publishing work the same way the scheduler does must release the worker.
	deadline := time.Now().Add(time.Second)
	for scheduler.idleParks.Load() == 0 {
		require.True(t, time.Now().Before(deadline), "worker never parked")
		time.Sleep(time.Millisecond)
	}
	scheduler.completeExecution(0)

	select {
	case <-returned:
	case <-time.After(10 * time.Second):
		t.Fatal("a notification published after the progress check was lost")
	}
}

// TestWorkerPoolReturnsCapacity pins that a resumed worker gives its
// replacement's capacity back instead of growing the pool without bound.
func TestWorkerPoolReturnsCapacity(t *testing.T) {
	pool := newWorkerPool(2, 16)
	spawns := 0
	pool.spawn = func() { spawns++ }

	pool.onPark()
	pool.onPark()
	require.Equal(t, 2, spawns, "each park below the target must be replaced")
	require.False(t, pool.retire(), "runnable workers are still at the target")

	pool.onUnpark()
	pool.onUnpark()
	require.EqualValues(t, 4, pool.peakRunnable(), "resuming both workers overshoots the target")
	require.True(t, pool.retire(), "the surplus must be retired")
	require.True(t, pool.retire(), "the surplus must be retired")
	require.False(t, pool.retire(), "retiring must stop at the configured target")
}
