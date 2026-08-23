package block_stm

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	storetypes "cosmossdk.io/store/types"
)

// This file is an additive extension of the frozen upstream kernel. It does
// not modify any original upstream file. It makes three implementation
// choices that upstream hard-codes into explicit, measurable policies:
//
//  1. EstimateReadPolicy - what a transaction does when a read observes an
//     ESTIMATE mark left by a lower-index transaction. Upstream always
//     suspends the executing goroutine on a condition variable, which
//     preserves the partial execution but removes a worker from the pool for
//     the whole wait. The Block-STM paper (Algorithm 1) instead aborts the
//     incarnation, registers a dependency and returns the worker to the
//     scheduler, paying a full re-execution instead.
//
//  2. IdleWaitPolicy - what a worker does when the scheduler has no task for
//     it. Upstream spins through NextTask/CheckDone with runtime.Gosched(),
//     which burns CPU that productive workers need whenever a large fraction
//     of the pool is blocked.
//
//  3. DispatchPolicy - whether transactions with known-unsatisfied static
//     predecessors are dispatched at all. Upstream dispatches strictly in
//     execution_idx order, so a consumer of static dependency information can
//     only block after the worker has already been committed to the
//     transaction. A ready-queue dispatcher defers such a transaction instead
//     and lets the worker take other work.
//
// The zero value of KernelPolicy reproduces upstream behaviour exactly and
// routes execution through the untouched upstream entry points, so existing
// results remain byte-for-byte reproducible.

// EstimateReadPolicy selects the response to reading an ESTIMATE mark.
type EstimateReadPolicy string

const (
	// EstimateReadSuspendInPlace is the frozen upstream behaviour: the
	// transaction keeps its partial execution and its worker, and resumes in
	// place once the blocking transaction finishes.
	EstimateReadSuspendInPlace EstimateReadPolicy = "suspend_in_place"
	// EstimateReadAbortReschedule is the Block-STM paper behaviour: the
	// incarnation is discarded, a dependency is registered, and the worker
	// immediately returns to the scheduler. The transaction is re-executed
	// from its first instruction once the blocking transaction finishes.
	EstimateReadAbortReschedule EstimateReadPolicy = "abort_and_reschedule"
	// EstimateReadSuspendYieldWorker keeps the partial execution like
	// suspend_in_place, but releases the worker permit before parking and
	// re-acquires one after waking, so the freed capacity is usable by other
	// transactions. It requires the parked-idle wait policy.
	EstimateReadSuspendYieldWorker EstimateReadPolicy = "suspend_yield_worker"
)

// IdleWaitPolicy selects what a worker does when no task is available.
type IdleWaitPolicy string

const (
	// IdleWaitGosched is the frozen upstream behaviour: busy-wait with
	// runtime.Gosched() between attempts.
	IdleWaitGosched IdleWaitPolicy = "gosched"
	// IdleWaitPark blocks the worker on a wake channel until scheduler state
	// changes, with a bounded safety interval so no wakeup can be lost.
	IdleWaitPark IdleWaitPolicy = "park"
)

// DispatchPolicy selects how execution tasks are handed to workers.
type DispatchPolicy string

const (
	// DispatchIndexOrder is the frozen upstream behaviour: hand out
	// transactions strictly in increasing execution_idx order.
	DispatchIndexOrder DispatchPolicy = "index_order"
	// DispatchReadyQueue defers a transaction whose declared predecessors are
	// incomplete instead of dispatching it, and releases it onto a ready
	// queue when those predecessors finish.
	DispatchReadyQueue DispatchPolicy = "ready_queue"
)

// idleParkSafetyInterval bounds how long a parked worker can sleep without an
// explicit wakeup. Every state change notifies the wake channel, so this is a
// backstop against a missed notification, not the normal wakeup path.
var idleParkSafetyInterval = 200 * time.Microsecond

var (
	// ErrKernelPolicyUnknown reports an unrecognised policy value.
	ErrKernelPolicyUnknown = errors.New("unknown kernel execution policy value")
	// ErrKernelPolicyGate reports missing or malformed ready-queue inputs.
	ErrKernelPolicyGate = errors.New("invalid ready-queue dispatch gate")
	// ErrKernelPolicyInflight reports the unimplemented combination of a
	// finite speculation window with a non-default kernel policy.
	ErrKernelPolicyInflight = errors.New("kernel execution policy does not support a finite speculation window")
)

// KernelPolicy configures the additive execution path. The zero value means
// "reproduce the frozen upstream kernel exactly".
type KernelPolicy struct {
	EstimateRead EstimateReadPolicy
	IdleWait     IdleWaitPolicy
	Dispatch     DispatchPolicy

	// Predecessors[i] lists transaction indices that must have completed an
	// execution before transaction i may be dispatched. Used by
	// DispatchReadyQueue when Barriers is nil.
	Predecessors [][]int
	// Barriers[i] is the highest index transaction i depends on; transaction
	// i becomes dispatchable once the contiguous completed frontier passes
	// it. A negative value means no barrier. Used by DispatchReadyQueue.
	Barriers []int
}

// KernelPolicyStats reports what the policy actually did during a block.
type KernelPolicyStats struct {
	Applied              bool   `json:"applied"`
	EstimateSuspends     uint64 `json:"estimate_suspends"`
	EstimateSuspendNS    uint64 `json:"estimate_suspend_ns"`
	EstimateAborts       uint64 `json:"estimate_aborts"`
	DispatchDeferrals    uint64 `json:"dispatch_deferrals"`
	WorkerYields         uint64 `json:"worker_yields"`
	PeakRunnableWorkers  uint64 `json:"peak_runnable_workers"`
	IdleParks            uint64 `json:"idle_parks"`
	ReadyQueueDispatches uint64 `json:"ready_queue_dispatches"`
}

// Normalize fills in defaults and validates the policy.
func (p KernelPolicy) Normalize() (KernelPolicy, error) {
	if p.EstimateRead == "" {
		p.EstimateRead = EstimateReadSuspendInPlace
	}
	if p.IdleWait == "" {
		p.IdleWait = IdleWaitGosched
		if p.EstimateRead == EstimateReadSuspendYieldWorker {
			p.IdleWait = IdleWaitPark
		}
	}
	if p.Dispatch == "" {
		p.Dispatch = DispatchIndexOrder
	}
	switch p.EstimateRead {
	case EstimateReadSuspendInPlace, EstimateReadAbortReschedule, EstimateReadSuspendYieldWorker:
	default:
		return p, fmt.Errorf("%w: estimate_read=%q", ErrKernelPolicyUnknown, p.EstimateRead)
	}
	switch p.IdleWait {
	case IdleWaitGosched, IdleWaitPark:
	default:
		return p, fmt.Errorf("%w: idle_wait=%q", ErrKernelPolicyUnknown, p.IdleWait)
	}
	if p.EstimateRead == EstimateReadSuspendYieldWorker && p.IdleWait != IdleWaitPark {
		// Yielding the worker is pointless if freed workers spin: the spinning
		// consumes the capacity the policy exists to release. Refused rather
		// than rewritten, because an explicit value is never overridden.
		return p, fmt.Errorf("%w: suspend_yield_worker requires idle_wait=park", ErrKernelPolicyGate)
	}
	switch p.Dispatch {
	case DispatchIndexOrder:
		if p.Predecessors != nil || p.Barriers != nil {
			return p, fmt.Errorf("%w: gate inputs supplied for index_order dispatch", ErrKernelPolicyGate)
		}
	case DispatchReadyQueue:
		if (p.Predecessors == nil) == (p.Barriers == nil) {
			return p, fmt.Errorf("%w: ready_queue needs exactly one of predecessors or barriers", ErrKernelPolicyGate)
		}
	default:
		return p, fmt.Errorf("%w: dispatch=%q", ErrKernelPolicyUnknown, p.Dispatch)
	}
	return p, nil
}

// IsFrozenDefault reports whether the policy reproduces upstream behaviour, in
// which case execution is routed through the untouched upstream entry points.
func (p KernelPolicy) IsFrozenDefault() bool {
	normalized, err := p.Normalize()
	if err != nil {
		return false
	}
	return normalized.EstimateRead == EstimateReadSuspendInPlace &&
		normalized.IdleWait == IdleWaitGosched &&
		normalized.Dispatch == DispatchIndexOrder
}

// ExecuteBlockWithKernelPolicy executes a block under an explicit kernel
// policy. A frozen-default policy delegates to the untouched upstream path.
func ExecuteBlockWithKernelPolicy(
	ctx context.Context,
	blockSize int,
	stores map[storetypes.StoreKey]int,
	storage MultiStore,
	executors int,
	maxInflight int,
	estimates []MultiLocations,
	policy KernelPolicy,
	txExecutor TxExecutor,
) (SpeculationStats, KernelPolicyStats, error) {
	normalized, err := policy.Normalize()
	if err != nil {
		return SpeculationStats{}, KernelPolicyStats{}, err
	}
	// Argument validation happens before the policy branch so both paths
	// reject the same inputs; the frozen entry point re-checks them itself.
	if blockSize < 0 {
		return SpeculationStats{}, KernelPolicyStats{}, fmt.Errorf("invalid block size: %d", blockSize)
	}
	if executors < 0 {
		return SpeculationStats{}, KernelPolicyStats{}, fmt.Errorf("invalid number of executors: %d", executors)
	}
	if maxInflight < 0 {
		return SpeculationStats{}, KernelPolicyStats{}, fmt.Errorf("invalid max speculative inflight: %d", maxInflight)
	}
	if len(estimates) > blockSize {
		return SpeculationStats{}, KernelPolicyStats{}, fmt.Errorf("estimate count %d exceeds block size %d", len(estimates), blockSize)
	}
	if normalized.IsFrozenDefault() {
		stats, err := ExecuteBlockWithMaxSpeculativeInflightAndEstimates(
			ctx, blockSize, stores, storage, executors, maxInflight, estimates, txExecutor,
		)
		return stats, KernelPolicyStats{}, err
	}
	if maxInflight > 0 && maxInflight < blockSize {
		// The admission limiter keeps its own stable-frontier bookkeeping in
		// speculation.go. Composing it with policy dispatch needs its own
		// correctness argument, so the combination is refused rather than
		// silently approximated.
		return SpeculationStats{}, KernelPolicyStats{}, ErrKernelPolicyInflight
	}
	if executors == 0 {
		executors = maxParallelism()
	}
	if err := normalized.validateGate(blockSize); err != nil {
		return SpeculationStats{}, KernelPolicyStats{}, err
	}

	scheduler := newPolicyScheduler(blockSize, normalized)
	mvMemory := NewMVMemoryWithEstimates(blockSize, stores, storage, scheduler.base, estimates)
	pool := newWorkerPool(executors, blockSize)

	var workers sync.WaitGroup
	spawn := func() {
		workers.Add(1)
		go func() {
			defer workers.Done()
			newPolicyExecutor(ctx, scheduler, txExecutor, mvMemory, pool).run()
		}()
	}
	pool.spawn = spawn
	for worker := 0; worker < executors; worker++ {
		spawn()
	}
	workers.Wait()

	stats := scheduler.stats(pool)
	if !scheduler.base.Done() {
		if ctx.Err() != nil {
			return SpeculationStats{}, stats, ctx.Err()
		}
		return SpeculationStats{}, stats, errors.New("policy scheduler did not complete")
	}
	mvMemory.WriteSnapshot(storage)
	return SpeculationStats{EffectiveLimit: uint64(blockSize)}, stats, nil
}

func (p KernelPolicy) validateGate(blockSize int) error {
	if p.Dispatch != DispatchReadyQueue {
		return nil
	}
	if p.Predecessors != nil {
		if len(p.Predecessors) != blockSize {
			return fmt.Errorf("%w: predecessors length %d != block size %d", ErrKernelPolicyGate, len(p.Predecessors), blockSize)
		}
		for txn, list := range p.Predecessors {
			for _, predecessor := range list {
				if predecessor < 0 || predecessor >= txn {
					// Edges must point from a lower to a higher index so the
					// gate can never introduce a cycle or reorder the preset
					// serialization order.
					return fmt.Errorf("%w: predecessor %d of transaction %d is not a lower index", ErrKernelPolicyGate, predecessor, txn)
				}
			}
		}
		return nil
	}
	if len(p.Barriers) != blockSize {
		return fmt.Errorf("%w: barriers length %d != block size %d", ErrKernelPolicyGate, len(p.Barriers), blockSize)
	}
	for txn, barrier := range p.Barriers {
		if barrier >= txn {
			return fmt.Errorf("%w: barrier %d of transaction %d is not a lower index", ErrKernelPolicyGate, barrier, txn)
		}
	}
	return nil
}
