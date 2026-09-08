package block_stm

import (
	"context"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

// policyScheduler wraps the frozen *Scheduler with the bookkeeping the
// additive policies need: a re-dispatch queue for transactions that were
// deferred or aborted, a wake channel so idle workers can park instead of
// spin, and the dependency gate used by ready-queue dispatch.
//
// All frozen invariants are preserved. Transactions still commit in preset
// order, every read is still validated by the untouched MVMemory validator,
// and the gate only ever delays a dispatch: it can never reorder the
// serialization order because every gate edge points from a lower to a higher
// transaction index.
type policyScheduler struct {
	base   *Scheduler
	policy KernelPolicy

	mu   sync.Mutex
	wake chan struct{}
	// nil keeps the existing full-window path and its telemetry semantics.
	admission *admissionWindow

	// ready holds transactions that must be re-dispatched outside the
	// monotonic execution_idx scan: gate-deferred transactions whose
	// predecessors have completed, and aborted transactions whose blocking
	// transaction has finished.
	ready []TxnIndex

	// dispatch gate state
	indegree   []int
	successors [][]TxnIndex
	deferred   []bool
	completed  []bool
	frontier   int
	released   int
	barrierOf  map[int][]TxnIndex

	// abort_and_reschedule state: blocking txn -> aborted dependents
	abortDeps [][]TxnIndex

	estimateSuspends  atomic.Uint64
	estimateSuspendNS atomic.Uint64
	estimateAborts    atomic.Uint64
	deferrals         atomic.Uint64
	workerYields      atomic.Uint64
	idleParks         atomic.Uint64
	readyDispatches   atomic.Uint64
}

func newPolicyScheduler(blockSize int, policy KernelPolicy) *policyScheduler {
	s := &policyScheduler{
		base:      NewScheduler(blockSize),
		policy:    policy,
		wake:      make(chan struct{}),
		completed: make([]bool, blockSize),
		abortDeps: make([][]TxnIndex, blockSize),
	}
	if policy.Dispatch == DispatchReadyQueue {
		s.deferred = make([]bool, blockSize)
		if policy.Predecessors != nil {
			s.indegree = make([]int, blockSize)
			s.successors = make([][]TxnIndex, blockSize)
			for txn, list := range policy.Predecessors {
				s.indegree[txn] = len(list)
				for _, predecessor := range list {
					s.successors[predecessor] = append(s.successors[predecessor], TxnIndex(txn))
				}
			}
		} else {
			s.barrierOf = make(map[int][]TxnIndex)
		}
	}
	return s
}

// notifyLocked wakes every parked worker. The caller must hold s.mu.
func (s *policyScheduler) notifyLocked() {
	close(s.wake)
	s.wake = make(chan struct{})
}

func (s *policyScheduler) notify() {
	s.mu.Lock()
	s.notifyLocked()
	s.mu.Unlock()
}

// checkDone runs the frozen completion check and wakes parked workers when the
// block finishes, so no worker can sleep past the end of the block.
func (s *policyScheduler) checkDone() {
	s.base.CheckDone()
	if s.base.Done() {
		s.notify()
	}
}

func (s *policyScheduler) popReady() (TxnIndex, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.ready) == 0 {
		return 0, false
	}
	txn := s.ready[len(s.ready)-1]
	s.ready = s.ready[:len(s.ready)-1]
	return txn, true
}

// gateReadyLocked reports whether the dispatch gate allows transaction txn.
func (s *policyScheduler) gateReadyLocked(txn TxnIndex) bool {
	if s.policy.Dispatch != DispatchReadyQueue {
		return true
	}
	if s.indegree != nil {
		return s.indegree[txn] == 0
	}
	return s.policy.Barriers[txn] < s.frontier
}

// deferLocked parks a transaction outside the dispatch scan. Its
// num_active_tasks reservation is intentionally retained so the frozen
// CheckDone cannot declare the block finished while the transaction is still
// pending.
func (s *policyScheduler) deferLocked(txn TxnIndex) {
	s.deferred[txn] = true
	s.deferrals.Add(1)
	if s.indegree == nil {
		barrier := s.policy.Barriers[txn]
		s.barrierOf[barrier] = append(s.barrierOf[barrier], txn)
	}
}

// completeExecution records that a transaction finished an execution and
// releases everything that was waiting on it.
func (s *policyScheduler) completeExecution(txn TxnIndex) {
	s.mu.Lock()
	if dependents := s.abortDeps[txn]; len(dependents) > 0 {
		s.abortDeps[txn] = nil
		// The discarded incarnations were already made ready by registerAbort.
		s.ready = append(s.ready, dependents...)
	}

	if s.policy.Dispatch == DispatchReadyQueue && !s.completed[txn] {
		s.completed[txn] = true
		if s.indegree != nil {
			for _, successor := range s.successors[txn] {
				s.indegree[successor]--
				if s.indegree[successor] == 0 && s.deferred[successor] {
					s.deferred[successor] = false
					s.ready = append(s.ready, successor)
				}
			}
		} else {
			for s.frontier < len(s.completed) && s.completed[s.frontier] {
				s.frontier++
			}
			for s.released < s.frontier {
				for _, waiting := range s.barrierOf[s.released] {
					if s.deferred[waiting] {
						s.deferred[waiting] = false
						s.ready = append(s.ready, waiting)
					}
				}
				delete(s.barrierOf, s.released)
				s.released++
			}
		}
	}

	s.notifyLocked()
	s.mu.Unlock()
}

// blockingResolved reports that the blocking transaction already finished, in
// which case the reader simply re-reads and continues, exactly like the
// paper's optimisation for a false return from add_dependency.
func (s *policyScheduler) blockingResolved(blocking TxnIndex) bool {
	ok, _ := s.base.txn_status[blocking].IsExecuted()
	return ok
}

// registerAbort makes an aborted transaction eligible again. It runs after the
// discarded attempt has fully unwound, so the attempt is always accounted for
// before another worker can pick the transaction up.
func (s *policyScheduler) registerAbort(txn, blocking TxnIndex) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.estimateAborts.Add(1)
	s.base.txn_status[txn].setStatus(StatusAborting)
	s.base.txn_status[txn].SetReadyStatus()
	if ok, _ := s.base.txn_status[blocking].IsExecuted(); ok {
		// Resolved while this attempt was unwinding: re-dispatch directly,
		// because the completion that would have released it already ran.
		s.ready = append(s.ready, txn)
		s.notifyLocked()
		return
	}
	s.abortDeps[blocking] = append(s.abortDeps[blocking], txn)
}

// nextTask returns the next task for a worker. It first drains the re-dispatch
// queue, then falls back to the frozen index-order scan with an optional
// dispatch gate.
// scanWork reports whether the frozen index scan can still yield a validation
// or an execution task. Both indices are compared against the block size
// because concurrent workers push execution_idx past it; without that clamp an
// exhausted validation scan would look like pending work forever and starve
// the re-dispatch queue.
func (s *policyScheduler) scanWork() (validate, execute bool) {
	size := uint64(s.base.block_size)
	validationIdx := s.base.validation_idx.Load()
	executionIdx := s.base.execution_idx.Load()
	return validationIdx < executionIdx && validationIdx < size, executionIdx < size
}

// nextTask drains the re-dispatch queue first, then follows the frozen
// validate-before-execute rule.
//
// Draining first does deviate from the frozen order, deliberately. The frozen
// rule keeps workers from running ahead of the validated prefix, and a
// released transaction is not ahead of it: the scan already reached that index
// and deferred it, so dispatching it is prefix work. Validation cannot be
// starved by it either, because entries are only added by completions and
// every pop removes one, so the queue drains.
//
// The strict alternative — validate whenever validation_idx lags — was
// measured and is far worse: each completion that writes a new path calls
// DecreaseValidationIdx, restarting the validation scan and starving
// re-dispatch behind it. On the selective-read-set smoke it cost 9.93x the
// runtime baseline against 2.93x for this order.
func (s *policyScheduler) nextTask() (TxnVersion, TaskKind) {
	if txn, ok := s.popReady(); ok {
		// These transactions were already admitted. Their index remains in
		// the window across deferral/abort, so no new slot is acquired here.
		if incarnation, started := s.base.txn_status[txn].TrySetExecuting(); started {
			s.readyDispatches.Add(1)
			return TxnVersion{txn, incarnation}, TaskKindExecution
		}
		// Another path already took the transaction; release the reservation
		// that was made when it was first dispatched.
		DecrAtomic(&s.base.num_active_tasks)
		return InvalidTxnVersion, TaskKindExecution
	}
	if validate, _ := s.scanWork(); validate {
		return s.nextVersionToValidate(), TaskKindValidation
	}
	return s.nextVersionToExecute(), TaskKindExecution
}

func (s *policyScheduler) nextVersionToValidate() TxnVersion {
	if s.admission != nil {
		// Serialize the scan with new-write-path invalidation, as in the
		// legacy admission scheduler.
		s.mu.Lock()
		version := s.base.NextVersionToValidate()
		s.mu.Unlock()
		if s.base.Done() {
			s.notify()
		}
		return version
	}
	if s.base.validation_idx.Load() >= uint64(s.base.block_size) {
		s.checkDone()
		return InvalidTxnVersion
	}
	IncrAtomic(&s.base.num_active_tasks)
	idx := FetchIncr(&s.base.validation_idx)
	if idx < uint64(s.base.block_size) {
		if ok, incarnation := s.base.txn_status[idx].IsExecuted(); ok {
			return TxnVersion{TxnIndex(idx), incarnation}
		}
	}
	DecrAtomic(&s.base.num_active_tasks)
	return InvalidTxnVersion
}

func (s *policyScheduler) nextVersionToExecute() TxnVersion {
	if s.base.execution_idx.Load() >= uint64(s.base.block_size) {
		s.checkDone()
		return InvalidTxnVersion
	}
	var idx TxnIndex
	if s.admission != nil {
		s.mu.Lock()
		next := int(s.base.execution_idx.Load())
		if next >= s.base.block_size || !s.admission.admit(next) {
			s.mu.Unlock()
			return InvalidTxnVersion
		}
		IncrAtomic(&s.base.num_active_tasks)
		idx = TxnIndex(s.base.execution_idx.Add(1) - 1)
		s.mu.Unlock()
	} else {
		IncrAtomic(&s.base.num_active_tasks)
		idx = TxnIndex(s.base.execution_idx.Add(1) - 1)
	}
	if int(idx) >= s.base.block_size {
		DecrAtomic(&s.base.num_active_tasks)
		return InvalidTxnVersion
	}
	if s.policy.Dispatch == DispatchReadyQueue {
		s.mu.Lock()
		if !s.gateReadyLocked(idx) {
			s.deferLocked(idx)
			s.mu.Unlock()
			// Reservation retained on purpose; the transaction is pending.
			return InvalidTxnVersion
		}
		s.mu.Unlock()
	}
	if incarnation, started := s.base.txn_status[idx].TrySetExecuting(); started {
		return TxnVersion{idx, incarnation}
	}
	DecrAtomic(&s.base.num_active_tasks)
	return InvalidTxnVersion
}

func (s *policyScheduler) finishExecution(version TxnVersion, wroteNewPath bool) (TxnVersion, TaskKind) {
	if s.admission != nil {
		s.mu.Lock()
		if wroteNewPath && s.base.validation_idx.Load() > uint64(version.Index) {
			s.admission.invalidateFrom(int(version.Index), int(s.base.execution_idx.Load()))
		}
	}
	next, kind := s.base.FinishExecution(version, wroteNewPath)
	if s.admission != nil {
		s.mu.Unlock()
	}
	s.completeExecution(version.Index)
	return next, kind
}

func (s *policyScheduler) validationToken(txn TxnIndex) uint64 {
	if s.admission == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.admission.epoch[txn]
}

func (s *policyScheduler) finishValidation(version TxnVersion, valid, aborted bool, token uint64) (TxnVersion, TaskKind) {
	if s.admission == nil {
		next, kind := s.base.FinishValidation(version.Index, aborted)
		s.notify()
		return next, kind
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if aborted {
		s.admission.invalidateFrom(int(version.Index), int(s.base.execution_idx.Load()))
	}
	next, kind := s.base.FinishValidation(version.Index, aborted)
	if valid && !aborted {
		executed, incarnation := s.base.txn_status[version.Index].IsExecuted()
		if executed && incarnation == version.Incarnation {
			s.admission.pass(version.Index, token)
		}
	}
	s.notifyLocked()
	return next, kind
}

// idleWait is the replacement for the frozen busy-wait. Under IdleWaitPark a
// worker sleeps until scheduler state changes.
//
// Every progress source is re-checked while holding the lock that all
// notifications are published under, so a wake cannot be lost between the
// check and the park, and a finished block is never slept on. The bounded
// safety interval remains only as a backstop for frozen kernel paths that
// change state without a notification this extension can hook.
func (s *policyScheduler) idleWait(ctx context.Context) {
	if s.policy.IdleWait == IdleWaitGosched && s.admission == nil {
		runtime.Gosched()
		return
	}
	s.mu.Lock()
	if s.base.Done() {
		s.mu.Unlock()
		return
	}
	// A failed attempt consumed one index, so unexamined execution or
	// validation work may still be dispatchable; scanning on is the progress
	// path and must not be delayed by a park.
	validate, execute := s.scanWork()
	windowBlocked := execute && s.admission != nil && !s.admission.allows(int(s.base.execution_idx.Load()))
	execute = execute && !windowBlocked
	if validate || execute || len(s.ready) > 0 {
		s.mu.Unlock()
		runtime.Gosched()
		return
	}
	wake := s.wake
	s.mu.Unlock()
	if windowBlocked {
		started := time.Now()
		defer func() { s.admission.recordStall(time.Since(started)) }()
	}
	if s.policy.IdleWait == IdleWaitGosched {
		runtime.Gosched()
		return
	}
	s.idleParks.Add(1)
	timer := time.NewTimer(idleParkSafetyInterval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-wake:
	case <-timer.C:
	}
}

func (s *policyScheduler) speculationStats() SpeculationStats {
	if s.admission == nil {
		return SpeculationStats{EffectiveLimit: uint64(s.base.block_size)}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.admission.stats()
}

func (s *policyScheduler) stats(pool *workerPool) KernelPolicyStats {
	return KernelPolicyStats{
		Applied:              true,
		EstimateSuspends:     s.estimateSuspends.Load(),
		EstimateSuspendNS:    s.estimateSuspendNS.Load(),
		EstimateAborts:       s.estimateAborts.Load(),
		DispatchDeferrals:    s.deferrals.Load(),
		WorkerYields:         s.workerYields.Load(),
		PeakRunnableWorkers:  pool.peakRunnable(),
		IdleParks:            s.idleParks.Load(),
		ReadyQueueDispatches: s.readyDispatches.Load(),
	}
}
