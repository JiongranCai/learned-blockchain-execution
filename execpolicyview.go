package block_stm

import (
	"io"
	"time"

	"cosmossdk.io/store/cachekv"
	"cosmossdk.io/store/tracekv"
	storetypes "cosmossdk.io/store/types"
)

var (
	_ storetypes.KVStore    = (*policyView[[]byte])(nil)
	_ storetypes.ObjKVStore = (*policyView[any])(nil)
	_ MVView                = (*policyView[[]byte])(nil)
	_ MVView                = (*policyView[any])(nil)
)

// estimateAbort unwinds a transaction whose read observed an ESTIMATE mark
// under the abort_and_reschedule policy. It is recovered by policyExecutor and
// never escapes the kernel.
type estimateAbort struct {
	blocking TxnIndex
}

// policyView reuses the frozen GMVMemoryView for everything except the
// response to an ESTIMATE read, which is the single decision this extension
// makes configurable. The read version selection, read-set recording and
// write-set handling are unchanged, so the mandatory MVMemory validation still
// applies to every policy.
type policyView[V any] struct {
	*GMVMemoryView[V]
	sched *policyScheduler
	pool  *workerPool
}

func wrapPolicyView(base MVView, sched *policyScheduler, pool *workerPool) MVView {
	switch view := base.(type) {
	case *GMVMemoryView[[]byte]:
		return &policyView[[]byte]{GMVMemoryView: view, sched: sched, pool: pool}
	case *GMVMemoryView[any]:
		return &policyView[any]{GMVMemoryView: view, sched: sched, pool: pool}
	default:
		panic("unsupported multi-version view value type")
	}
}

// Get repeats the frozen read logic with a policy-aware estimate response.
func (v *policyView[V]) Get(key []byte) V {
	s := v.GMVMemoryView
	if s.writeSet != nil {
		if value, found := s.writeSet.OverlayGet(key); found {
			// value written by this txn; nil value means deleted
			return value
		}
	}

	for {
		value, version, estimate := s.mvData.Read(key, s.txn)
		if estimate {
			v.onEstimateRead(version.Index)
			continue
		}

		s.readSet.Reads = append(s.readSet.Reads, ReadDescriptor{key, version})
		if !version.Valid() {
			return s.storage.Get(key)
		}
		return value
	}
}

func (v *policyView[V]) Has(key []byte) bool {
	return !v.GMVMemoryView.mvData.isZero(v.Get(key))
}

// onEstimateRead applies the configured response to an ESTIMATE mark left by
// the blocking transaction.
func (v *policyView[V]) onEstimateRead(blocking TxnIndex) {
	s := v.GMVMemoryView
	if v.sched.policy.EstimateRead == EstimateReadAbortReschedule {
		if v.sched.blockingResolved(blocking) {
			// The blocking transaction already finished, so the paper's
			// add_dependency false path applies: re-read and continue.
			return
		}
		// Registration happens in the executor once this attempt has unwound.
		panic(estimateAbort{blocking: blocking})
	}

	cond := s.scheduler.WaitForDependency(s.txn, blocking)
	if cond == nil {
		return
	}
	yield := v.sched.policy.EstimateRead == EstimateReadSuspendYieldWorker
	if yield {
		// Release this worker before parking so the freed capacity is usable
		// by other transactions, then take it back after waking.
		v.pool.onPark()
		v.sched.workerYields.Add(1)
	}
	started := time.Now()
	cond.Wait()
	v.sched.estimateSuspendNS.Add(uint64(time.Since(started)))
	v.sched.estimateSuspends.Add(1)
	if yield {
		v.pool.onUnpark()
	}
}

// iterationSupported reports whether range reads keep frozen semantics. The
// frozen iterator resolves estimates through GMVMemoryView.waitFor, which
// always suspends in place, so the non-default estimate policies would not
// apply to keys reached through an iterator.
func (v *policyView[V]) iterationSupported() bool {
	return v.sched.policy.EstimateRead == EstimateReadSuspendInPlace
}

func (v *policyView[V]) Iterator(start, end []byte) storetypes.GIterator[V] {
	if !v.iterationSupported() {
		panic("block_stm: iteration is unavailable under estimate_read=" + string(v.sched.policy.EstimateRead))
	}
	return v.GMVMemoryView.Iterator(start, end)
}

func (v *policyView[V]) ReverseIterator(start, end []byte) storetypes.GIterator[V] {
	if !v.iterationSupported() {
		panic("block_stm: reverse iteration is unavailable under estimate_read=" + string(v.sched.policy.EstimateRead))
	}
	return v.GMVMemoryView.ReverseIterator(start, end)
}

// CacheWrap wraps this view rather than the embedded one so cached reads keep
// the configured estimate policy.
func (v *policyView[V]) CacheWrap() storetypes.CacheWrap {
	return cachekv.NewGStore[V](v, v.GMVMemoryView.mvData.isZero, v.GMVMemoryView.mvData.valueLen)
}

func (v *policyView[V]) CacheWrapWithTrace(w io.Writer, tc storetypes.TraceContext) storetypes.CacheWrap {
	if store, ok := any(v).(*policyView[[]byte]); ok {
		return cachekv.NewGStore(tracekv.NewStore(store, w, tc), store.GMVMemoryView.mvData.isZero, store.GMVMemoryView.mvData.valueLen)
	}
	return v.CacheWrap()
}
