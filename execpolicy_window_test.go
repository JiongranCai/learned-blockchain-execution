package block_stm

import (
	"context"
	"strconv"
	"testing"
	"time"

	storetypes "cosmossdk.io/store/types"
	"github.com/test-go/testify/require"
)

func TestPolicyWindowRetainsDeferredAndAbortedTransactions(t *testing.T) {
	s := newPolicyScheduler(4, KernelPolicy{Dispatch: DispatchReadyQueue,
		Predecessors: [][]int{{}, {0}, {}, {}}, IdleWait: IdleWaitPark})
	s.admission = newAdmissionWindow(4, 2)
	head := s.nextVersionToExecute()
	require.Equal(t, TxnIndex(0), head.Index)
	require.False(t, s.nextVersionToExecute().Valid()) // T1 waits for T0.
	require.False(t, s.nextVersionToExecute().Valid()) // T2 is outside L=2.
	require.True(t, s.deferred[1])
	require.Equal(t, uint64(2), s.base.execution_idx.Load())

	s.finishExecution(head, true)
	dependent, _ := s.nextTask() // Ready queue may run T1 before T0 is validated.
	require.Equal(t, TxnIndex(1), dependent.Index)
	s.registerAbort(dependent.Index, head.Index)
	retry, _ := s.nextTask()
	require.Equal(t, dependent.Index, retry.Index)
	require.Equal(t, dependent.Incarnation+1, retry.Incarnation)
	require.False(t, s.nextVersionToExecute().Valid())
	require.Equal(t, uint64(2), s.base.execution_idx.Load())

	validation := s.nextVersionToValidate()
	require.Equal(t, head, validation)
	s.finishValidation(validation, true, false, s.validationToken(validation.Index))
	require.Equal(t, 1, s.admission.stable)
	require.Equal(t, TxnIndex(2), s.nextVersionToExecute().Index)
	require.False(t, s.nextVersionToExecute().Valid()) // T1 still occupies its slot.
	require.Equal(t, uint64(2), s.speculationStats().PeakInflight)
}

func TestPolicyWindowInvalidatesSuffixAndOldValidation(t *testing.T) {
	s := newPolicyScheduler(4, KernelPolicy{IdleWait: IdleWaitPark})
	s.admission = newAdmissionWindow(4, 2)
	head, tail := s.nextVersionToExecute(), s.nextVersionToExecute()
	s.finishExecution(tail, false)
	require.False(t, s.nextVersionToValidate().Valid()) // T0 is still executing.
	require.Equal(t, tail, s.nextVersionToValidate())
	s.finishValidation(tail, true, false, s.validationToken(tail.Index))
	require.True(t, s.admission.passed[1])
	require.False(t, s.nextVersionToExecute().Valid()) // Out-of-order pass releases no slot.

	// Another validation of T1 is in flight when T0 publishes a new write path.
	s.base.DecreaseValidationIdx(1)
	require.Equal(t, tail, s.nextVersionToValidate())
	oldToken := s.validationToken(1)
	s.finishExecution(head, true)
	require.False(t, s.admission.passed[1])
	s.finishValidation(tail, true, false, oldToken)
	require.False(t, s.admission.passed[1])

	require.Equal(t, head, s.nextVersionToValidate())
	s.finishValidation(head, true, false, s.validationToken(0))
	require.Equal(t, 1, s.admission.stable)
	require.Equal(t, tail, s.nextVersionToValidate())
	s.finishValidation(tail, true, false, s.validationToken(1))
	require.Equal(t, 2, s.admission.stable)
}

func TestPolicyWindowValidationAbortRetainsSlot(t *testing.T) {
	s := newPolicyScheduler(4, KernelPolicy{IdleWait: IdleWaitPark})
	s.admission = newAdmissionWindow(4, 2)
	head, tail := s.nextVersionToExecute(), s.nextVersionToExecute()
	s.finishExecution(head, false)
	s.finishExecution(tail, false)
	require.Equal(t, head, s.nextVersionToValidate())
	require.Equal(t, tail, s.nextVersionToValidate())
	s.finishValidation(tail, true, false, s.validationToken(1))
	// A duplicate validation task can outlive the incarnation it was assigned.
	s.base.DecreaseValidationIdx(0)
	require.Equal(t, head, s.nextVersionToValidate())
	require.True(t, s.base.TryValidationAbort(head))
	retry, kind := s.finishValidation(head, false, true, s.validationToken(0))
	require.Equal(t, TaskKindExecution, kind)
	require.Equal(t, head.Incarnation+1, retry.Incarnation)
	require.False(t, s.admission.passed[1])
	// Even a fresh epoch token cannot make the old incarnation stable.
	s.finishValidation(head, true, false, s.validationToken(0))
	require.Equal(t, 0, s.admission.stable)
	require.False(t, s.nextVersionToExecute().Valid())
	require.Equal(t, uint64(2), s.base.execution_idx.Load())
}

func TestPolicyWindowParksAndWakesOnCancellation(t *testing.T) {
	previous := idleParkSafetyInterval
	idleParkSafetyInterval = time.Second
	defer func() { idleParkSafetyInterval = previous }()
	s := newPolicyScheduler(4, KernelPolicy{IdleWait: IdleWaitPark})
	s.admission = newAdmissionWindow(4, 1)
	s.nextVersionToExecute()
	require.False(t, s.nextVersionToValidate().Valid()) // Exhaust the scan of the executing head.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { s.idleWait(ctx); close(done) }()
	deadline := time.Now().Add(time.Second)
	for s.idleParks.Load() == 0 {
		select {
		case <-done:
			t.Fatal("a full window was mistaken for executable scan work")
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("worker did not park")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	waitClosed(t, done)
	require.Equal(t, uint64(1), s.speculationStats().AdmissionStallEvents)
}

func TestPolicyFiniteWindowWithEstimatePolicies(t *testing.T) {
	stores := map[storetypes.StoreKey]int{StoreKeyAuth: 0, StoreKeyBank: 1}
	const size = 48
	estimates := make([]MultiLocations, size)
	for i := range estimates {
		estimates[i] = MultiLocations{0: Locations{Key(accountName(int64(i % 3)))}}
	}
	for _, entry := range policyMatrix() {
		for _, limit := range []int{1, 3} {
			t.Run(entry.name+"/L-"+strconv.Itoa(limit), func(t *testing.T) {
				block := testBlock(size, 3)
				storage := NewMultiMemDB(stores)
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				window, _, err := ExecuteBlockWithKernelPolicy(ctx, size, stores, storage,
					8, limit, estimates, entry.policy, block.ExecuteTx)
				require.NoError(t, err)
				require.True(t, window.TelemetryAvailable)
				require.True(t, window.PeakInflight > 0 && window.PeakInflight <= uint64(limit))
				reference := NewMultiMemDB(stores)
				runSequential(reference, testBlock(size, 3))
				for store := range stores {
					require.True(t, StoreEqual(reference.GetKVStore(store), storage.GetKVStore(store)))
				}
			})
		}
	}
}
