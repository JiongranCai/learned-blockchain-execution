package block_stm

import (
	"context"
	"sync"

	storetypes "cosmossdk.io/store/types"
)

// workerPool keeps the number of workers that can make progress equal to the
// configured executor count while the suspend_yield_worker policy has workers
// parked on a dependency. A parking worker is replaced, and a resuming worker
// gives that capacity back by letting the resulting surplus worker exit at its
// next scheduling point.
//
// Capacity is returned this way rather than by blocking the resuming worker on
// a permit. A blocking handoff can deadlock: workers waiting inside an
// index-order dependency gate hold their capacity and cannot release it, so a
// resuming worker whose transaction they are waiting for would wait forever.
//
// The bound is therefore target + (workers that resume before any of them
// reaches its next scheduling point), never the unbounded growth a pool
// without a handback would show. PeakRunnableWorkers reports what a run
// actually reached, so the overshoot stays auditable instead of hidden.
type workerPool struct {
	mu      sync.Mutex
	target  int
	limit   int
	spawned int
	parked  int
	peak    int
	spawn   func()
}

func newWorkerPool(executors, blockSize int) *workerPool {
	return &workerPool{
		target:  executors,
		limit:   executors + blockSize,
		spawned: executors,
		peak:    executors,
	}
}

// observeLocked records the highest number of workers that were runnable at
// once. The caller must hold p.mu.
func (p *workerPool) observeLocked() {
	if runnable := p.spawned - p.parked; runnable > p.peak {
		p.peak = runnable
	}
}

// onPark records that a worker is about to block on a dependency and adds a
// replacement only when the number of unparked workers has fallen below the
// configured executor count.
func (p *workerPool) onPark() {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.parked++
	needed := p.spawned-p.parked < p.target && p.spawned < p.limit
	if needed {
		p.spawned++
	}
	p.observeLocked()
	spawn := p.spawn
	p.mu.Unlock()
	if needed && spawn != nil {
		spawn()
	}
}

func (p *workerPool) onUnpark() {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.parked--
	p.observeLocked()
	p.mu.Unlock()
}

// retire reports whether the calling worker should exit because resumed
// workers have pushed the pool above its configured size. It never retires the
// last runnable worker: the surplus test leaves at least target workers.
func (p *workerPool) retire() bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.spawned-p.parked <= p.target {
		return false
	}
	p.spawned--
	return true
}

func (p *workerPool) peakRunnable() uint64 {
	if p == nil {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return uint64(p.peak)
}

// policyExecutor mirrors the frozen Executor loop, replacing only the task
// acquisition, the idle wait and the estimate-read handling.
type policyExecutor struct {
	ctx        context.Context
	sched      *policyScheduler
	txExecutor TxExecutor
	mvMemory   *MVMemory
	pool       *workerPool
}

func newPolicyExecutor(
	ctx context.Context,
	sched *policyScheduler,
	txExecutor TxExecutor,
	mvMemory *MVMemory,
	pool *workerPool,
) *policyExecutor {
	return &policyExecutor{ctx: ctx, sched: sched, txExecutor: txExecutor, mvMemory: mvMemory, pool: pool}
}

func (e *policyExecutor) run() {
	version := InvalidTxnVersion
	var kind TaskKind
	for !e.sched.base.Done() {
		if !version.Valid() {
			select {
			case <-e.ctx.Done():
				return
			default:
			}
			if e.pool.retire() {
				return
			}
			version, kind = e.sched.nextTask()
			if !version.Valid() {
				e.sched.idleWait(e.ctx)
			}
			continue
		}
		switch kind {
		case TaskKindExecution:
			version, kind = e.tryExecute(version)
		case TaskKindValidation:
			version, kind = e.needsReexecution(version)
		}
	}
}

func (e *policyExecutor) tryExecute(version TxnVersion) (TxnVersion, TaskKind) {
	e.sched.base.executedTxns.Add(1)
	view := e.newView(version.Index)
	if blocking, aborted := e.runTransaction(version.Index, view); aborted {
		// The incarnation is discarded without publishing anything. Registering
		// the dependency here, rather than at the read, keeps the ordering
		// deterministic: the discarded attempt has fully unwound and been
		// accounted for before the transaction can be dispatched again.
		e.sched.registerAbort(version.Index, blocking)
		return InvalidTxnVersion, TaskKindExecution
	}
	wroteNewLocation := e.mvMemory.Record(version, view)
	return e.sched.finishExecution(version, wroteNewLocation)
}

// runTransaction runs the transaction callback and converts an estimate abort
// into a normal return. Any other panic is propagated unchanged.
func (e *policyExecutor) runTransaction(txn TxnIndex, view *MultiMVMemoryView) (blocking TxnIndex, aborted bool) {
	defer func() {
		if recovered := recover(); recovered != nil {
			if abort, ok := recovered.(estimateAbort); ok {
				blocking, aborted = abort.blocking, true
				return
			}
			panic(recovered)
		}
	}()
	e.txExecutor(txn, view)
	return 0, false
}

func (e *policyExecutor) needsReexecution(version TxnVersion) (TxnVersion, TaskKind) {
	e.sched.base.validatedTxns.Add(1)
	token := e.sched.validationToken(version.Index)
	valid := e.mvMemory.ValidateReadSet(version.Index)
	aborted := !valid && e.sched.base.TryValidationAbort(version)
	if aborted {
		e.mvMemory.ConvertWritesToEstimates(version.Index)
	}
	return e.sched.finishValidation(version, valid, aborted, token)
}

func (e *policyExecutor) newView(txn TxnIndex) *MultiMVMemoryView {
	return NewMultiMVMemoryView(e.mvMemory.stores, e.newPolicyView, txn)
}

func (e *policyExecutor) newPolicyView(name storetypes.StoreKey, txn TxnIndex) MVView {
	index := e.mvMemory.stores[name]
	base := NewMVView(index, e.mvMemory.storage.GetStore(name), e.mvMemory.GetMVStore(index), e.mvMemory.scheduler, txn)
	return wrapPolicyView(base, e.sched, e.pool)
}
