package control

// This file exposes the three Block-STM kernel implementation choices that the
// frozen upstream kernel hard-codes. They are ordinary CQ2/CQ3-U control
// choices: each one only changes when work happens, never what the block
// commits. The default of every knob reproduces the frozen upstream path.

// EstimateReadPolicy selects what a transaction does when a read observes an
// ESTIMATE mark left by a lower-index transaction.
//
//   - suspend_in_place keeps the partial execution and the worker. It is the
//     frozen upstream behaviour and wastes no completed work, but it removes a
//     worker from the pool for the whole wait.
//   - abort_and_reschedule is the Block-STM paper behaviour. It discards the
//     incarnation and frees the worker immediately, paying a full
//     re-execution.
//   - suspend_yield_worker keeps the partial execution and releases the worker
//     for the duration of the wait.
type EstimateReadPolicy string

const (
	EstimateReadSuspendInPlace     EstimateReadPolicy = "suspend_in_place"
	EstimateReadAbortReschedule    EstimateReadPolicy = "abort_and_reschedule"
	EstimateReadSuspendYieldWorker EstimateReadPolicy = "suspend_yield_worker"
)

func ValidEstimateReadPolicy(policy EstimateReadPolicy) bool {
	switch policy {
	case EstimateReadSuspendInPlace, EstimateReadAbortReschedule, EstimateReadSuspendYieldWorker:
		return true
	default:
		return false
	}
}

// IdleWaitPolicy selects what a worker does when the scheduler has no task.
// The frozen upstream kernel busy-waits with runtime.Gosched(), which consumes
// CPU that productive workers need whenever part of the pool is blocked.
type IdleWaitPolicy string

const (
	IdleWaitGosched IdleWaitPolicy = "gosched"
	IdleWaitPark    IdleWaitPolicy = "park"
)

func ValidIdleWaitPolicy(policy IdleWaitPolicy) bool {
	return policy == IdleWaitGosched || policy == IdleWaitPark
}

// DependencyDispatchPolicy selects how a static dependency representation is
// consumed. index_order keeps the frozen behaviour: the transaction is
// dispatched to a worker and the wait happens inside the transaction callback,
// so a waiting transaction occupies a worker. ready_queue defers a transaction
// whose predecessors are incomplete instead of dispatching it, which is the
// scheduling semantics a dependency DAG actually describes.
type DependencyDispatchPolicy string

const (
	DependencyDispatchIndexOrder DependencyDispatchPolicy = "index_order"
	DependencyDispatchReadyQueue DependencyDispatchPolicy = "ready_queue"
)

func ValidDependencyDispatchPolicy(policy DependencyDispatchPolicy) bool {
	return policy == DependencyDispatchIndexOrder || policy == DependencyDispatchReadyQueue
}

// KernelPolicyCounters reports what the kernel policies actually did. All
// durations are summed across workers and may exceed block wall time.
type KernelPolicyCounters struct {
	Applied              bool                     `json:"applied"`
	EstimateRead         EstimateReadPolicy       `json:"estimate_read"`
	IdleWait             IdleWaitPolicy           `json:"idle_wait"`
	Dispatch             DependencyDispatchPolicy `json:"dispatch"`
	EstimateSuspends     uint64                   `json:"estimate_suspends"`
	EstimateSuspendNS    uint64                   `json:"estimate_suspend_ns"`
	EstimateAborts       uint64                   `json:"estimate_aborts"`
	DispatchDeferrals    uint64                   `json:"dispatch_deferrals"`
	WorkerYields         uint64                   `json:"worker_yields"`
	PeakRunnableWorkers  uint64                   `json:"peak_runnable_workers"`
	IdleParks            uint64                   `json:"idle_parks"`
	ReadyQueueDispatches uint64                   `json:"ready_queue_dispatches"`
}
