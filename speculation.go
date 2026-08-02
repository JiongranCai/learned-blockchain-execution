package block_stm

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	storetypes "cosmossdk.io/store/types"
)

// SpeculationStats describes the optional CQ2 admission limiter. A zero
// configured limit means the original full-block window (W). Exact peak and
// stall telemetry is available only when a finite limit below W is applied;
// the original ExecuteBlock path remains untouched for the B0 baseline.
type SpeculationStats struct {
	EffectiveLimit       uint64
	LimitApplied         bool
	TelemetryAvailable   bool
	PeakInflight         uint64
	AdmissionStallEvents uint64
	AdmissionStallNS     uint64
}

// ExecuteBlockWithMaxSpeculativeInflight executes a block with at most
// min(maxInflight, blockSize) admitted transactions beyond the continuous
// stable validated frontier. A transaction occupies one slot across
// execution, suspension, validation, and every incarnation; reexecution does
// not consume another slot.
//
// maxInflight == 0 is the B0 full-window default and calls ExecuteBlock
// directly. Values at least as large as blockSize do the same. This preserves
// the frozen upstream path when the limiter cannot constrain admission.
func ExecuteBlockWithMaxSpeculativeInflight(
	ctx context.Context,
	blockSize int,
	stores map[storetypes.StoreKey]int,
	storage MultiStore,
	executors int,
	maxInflight int,
	txExecutor TxExecutor,
) (SpeculationStats, error) {
	if blockSize < 0 {
		return SpeculationStats{}, fmt.Errorf("invalid block size: %d", blockSize)
	}
	if executors < 0 {
		return SpeculationStats{}, fmt.Errorf("invalid number of executors: %d", executors)
	}
	if maxInflight < 0 {
		return SpeculationStats{}, fmt.Errorf("invalid max speculative inflight: %d", maxInflight)
	}
	if executors == 0 {
		executors = maxParallelism()
	}

	effectiveLimit := blockSize
	if maxInflight > 0 && maxInflight < effectiveLimit {
		effectiveLimit = maxInflight
	}
	stats := SpeculationStats{EffectiveLimit: uint64(effectiveLimit)}
	if blockSize == 0 || maxInflight == 0 || maxInflight >= blockSize {
		return stats, ExecuteBlock(ctx, blockSize, stores, storage, executors, txExecutor)
	}

	scheduler := newAdmissionScheduler(blockSize, effectiveLimit)
	mvMemory := NewMVMemory(blockSize, stores, storage, scheduler.base)

	var workers sync.WaitGroup
	workers.Add(executors)
	for worker := 0; worker < executors; worker++ {
		executor := newAdmissionExecutor(ctx, scheduler, txExecutor, mvMemory)
		go func() {
			defer workers.Done()
			executor.run()
		}()
	}
	workers.Wait()

	stats = scheduler.stats()
	if !scheduler.base.Done() {
		if ctx.Err() != nil {
			return stats, ctx.Err()
		}
		return stats, errors.New("admission-limited scheduler did not complete")
	}
	mvMemory.WriteSnapshot(storage)
	return stats, nil
}

type admissionScheduler struct {
	base       *Scheduler
	limit      int
	mu         sync.Mutex
	stable     int
	passed     []bool
	epoch      []uint64
	generation uint64
	peak       uint64
	wake       chan struct{}

	stallEvents atomic.Uint64
	stallNS     atomic.Uint64
}

func newAdmissionScheduler(blockSize, limit int) *admissionScheduler {
	return &admissionScheduler{
		base:   NewScheduler(blockSize),
		limit:  limit,
		passed: make([]bool, blockSize),
		epoch:  make([]uint64, blockSize),
		wake:   make(chan struct{}),
	}
}

func (s *admissionScheduler) nextTask() (TxnVersion, TaskKind, <-chan struct{}) {
	validationIndex := s.base.validation_idx.Load()
	executionIndex := s.base.execution_idx.Load()
	if validationIndex < executionIndex {
		s.mu.Lock()
		version := s.base.NextVersionToValidate()
		s.mu.Unlock()
		return version, TaskKindValidation, nil
	}

	s.mu.Lock()
	next := int(s.base.execution_idx.Load())
	if next >= s.base.block_size {
		s.mu.Unlock()
		s.base.CheckDone()
		return InvalidTxnVersion, TaskKindExecution, nil
	}
	if next >= s.stable+s.limit {
		wake := s.wake
		s.mu.Unlock()
		return InvalidTxnVersion, TaskKindExecution, wake
	}
	IncrAtomic(&s.base.num_active_tasks)
	index := s.base.execution_idx.Add(1) - 1
	inflight := index + 1 - uint64(s.stable)
	if inflight > s.peak {
		s.peak = inflight
	}
	s.mu.Unlock()
	return s.base.TryIncarnate(TxnIndex(index)), TaskKindExecution, nil
}

func (s *admissionScheduler) finishExecution(version TxnVersion, wroteNewPath bool) (TxnVersion, TaskKind) {
	s.mu.Lock()
	if wroteNewPath && s.base.validation_idx.Load() > uint64(version.Index) {
		s.invalidateFromLocked(int(version.Index))
	}
	next, kind := s.base.FinishExecution(version, wroteNewPath)
	s.notifyLocked()
	s.mu.Unlock()
	return next, kind
}

func (s *admissionScheduler) validationToken(transaction TxnIndex) uint64 {
	s.mu.Lock()
	token := s.epoch[transaction]
	s.mu.Unlock()
	return token
}

func (s *admissionScheduler) finishValidation(
	version TxnVersion,
	valid bool,
	aborted bool,
	token uint64,
) (TxnVersion, TaskKind) {
	if aborted {
		s.mu.Lock()
		s.invalidateFromLocked(int(version.Index))
		s.notifyLocked()
		s.mu.Unlock()
	}

	next, kind := s.base.FinishValidation(version.Index, aborted)
	if valid && !aborted {
		s.mu.Lock()
		if s.epoch[version.Index] == token {
			s.passed[version.Index] = true
			before := s.stable
			for s.stable < len(s.passed) && s.passed[s.stable] {
				s.stable++
			}
			if s.stable != before {
				s.notifyLocked()
			}
		}
		s.mu.Unlock()
	}
	return next, kind
}

func (s *admissionScheduler) invalidateFromLocked(first int) {
	s.generation++
	admitted := min(int(s.base.execution_idx.Load()), s.base.block_size)
	for transaction := first; transaction < admitted; transaction++ {
		s.epoch[transaction] = s.generation
		s.passed[transaction] = false
	}
}

func (s *admissionScheduler) notifyLocked() {
	close(s.wake)
	s.wake = make(chan struct{})
}

func (s *admissionScheduler) recordStall(elapsed time.Duration) {
	s.stallEvents.Add(1)
	s.stallNS.Add(uint64(elapsed))
}

func (s *admissionScheduler) stats() SpeculationStats {
	s.mu.Lock()
	peak := s.peak
	s.mu.Unlock()
	return SpeculationStats{
		EffectiveLimit:       uint64(s.limit),
		LimitApplied:         true,
		TelemetryAvailable:   true,
		PeakInflight:         peak,
		AdmissionStallEvents: s.stallEvents.Load(),
		AdmissionStallNS:     s.stallNS.Load(),
	}
}

type admissionExecutor struct {
	ctx        context.Context
	scheduler  *admissionScheduler
	txExecutor TxExecutor
	mvMemory   *MVMemory
}

func newAdmissionExecutor(
	ctx context.Context,
	scheduler *admissionScheduler,
	txExecutor TxExecutor,
	mvMemory *MVMemory,
) *admissionExecutor {
	return &admissionExecutor{
		ctx:        ctx,
		scheduler:  scheduler,
		txExecutor: txExecutor,
		mvMemory:   mvMemory,
	}
}

func (e *admissionExecutor) run() {
	var kind TaskKind
	version := InvalidTxnVersion
	for !e.scheduler.base.Done() {
		if !version.Valid() {
			select {
			case <-e.ctx.Done():
				return
			default:
			}

			var wake <-chan struct{}
			version, kind, wake = e.scheduler.nextTask()
			if wake != nil {
				started := time.Now()
				select {
				case <-e.ctx.Done():
					e.scheduler.recordStall(time.Since(started))
					return
				case <-wake:
					e.scheduler.recordStall(time.Since(started))
				}
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

func (e *admissionExecutor) tryExecute(version TxnVersion) (TxnVersion, TaskKind) {
	e.scheduler.base.executedTxns.Add(1)
	view := e.mvMemory.View(version.Index)
	e.txExecutor(version.Index, view)
	wroteNewLocation := e.mvMemory.Record(version, view)
	return e.scheduler.finishExecution(version, wroteNewLocation)
}

func (e *admissionExecutor) needsReexecution(version TxnVersion) (TxnVersion, TaskKind) {
	e.scheduler.base.validatedTxns.Add(1)
	token := e.scheduler.validationToken(version.Index)
	valid := e.mvMemory.ValidateReadSet(version.Index)
	aborted := !valid && e.scheduler.base.TryValidationAbort(version)
	if aborted {
		e.mvMemory.ConvertWritesToEstimates(version.Index)
	}
	return e.scheduler.finishValidation(version, valid, aborted, token)
}
