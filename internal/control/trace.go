package control

import (
	"sort"
	"sync"
	"sync/atomic"
)

type TraceMode string

const (
	TraceDetailed TraceMode = "detailed"
	TraceCounters TraceMode = "counters"
	TraceOff      TraceMode = "off"
)

func ValidTraceMode(mode TraceMode) bool {
	return mode == TraceDetailed || mode == TraceCounters || mode == TraceOff
}

type EventRecord struct {
	Event              Event      `json:"event"`
	BlockID            string     `json:"block_id,omitempty"`
	TransactionID      string     `json:"transaction_id,omitempty"`
	TxIndex            uint64     `json:"tx_index,omitempty"`
	Incarnation        uint64     `json:"incarnation,omitempty"`
	Ordinal            uint64     `json:"ordinal,omitempty"`
	TargetID           string     `json:"target_id,omitempty"`
	Action             string     `json:"action"`
	TrustClass         TrustClass `json:"trust_class"`
	FeatureSource      string     `json:"feature_source"`
	ObservationVersion string     `json:"observation_version"`
	PolicyTableVersion string     `json:"policy_table_version"`
	DecisionDurationNS uint64     `json:"decision_duration_ns"`
}

type ActionCounter struct {
	Event  Event  `json:"event"`
	Action string `json:"action"`
	Count  uint64 `json:"count"`
}

type Trace struct {
	Engine                   string          `json:"engine"`
	PolicyName               string          `json:"policy_name"`
	PolicyVersion            string          `json:"policy_version"`
	Mode                     TraceMode       `json:"mode"`
	Events                   []EventRecord   `json:"events,omitempty"`
	ActionCounters           []ActionCounter `json:"action_counters,omitempty"`
	FallbackCounters         []ActionCounter `json:"fallback_counters,omitempty"`
	PolicyDecisionDurationNS uint64          `json:"policy_decision_duration_ns"`
	WorkAvailable            bool            `json:"work_available"`
	Work                     WorkCounters    `json:"work"`
}

type WorkCounters struct {
	ExecutionAttempts             uint64 `json:"execution_attempts"`
	ReexecutionAttempts           uint64 `json:"reexecution_attempts"`
	UsefulExecutionUnits          uint64 `json:"useful_execution_units"`
	ReexecutedExecutionUnits      uint64 `json:"reexecuted_execution_units"`
	DiscardedExecutionUnits       uint64 `json:"discarded_execution_units"`
	SpeculationLimit              uint64 `json:"speculation_limit"`
	SpeculationLimitApplied       bool   `json:"speculation_limit_applied"`
	SpeculationTelemetryAvailable bool   `json:"speculation_telemetry_available"`
	PeakSpeculativeInflight       uint64 `json:"peak_speculative_inflight"`
	AdmissionStallEvents          uint64 `json:"admission_stall_events"`
	AdmissionStallNS              uint64 `json:"admission_stall_ns"`
}

// Recorder accepts concurrent event emissions. Snapshot returns a stable
// logical order rather than goroutine completion order.
type Recorder struct {
	mode               TraceMode
	recordsMu          sync.Mutex
	records            []EventRecord
	counters           [knownCounterCount]atomic.Uint64
	decisionDurationNS atomic.Uint64
	unknownMu          sync.Mutex
	unknownCounters    map[counterKey]ActionCounter
}

type counterKey struct {
	event  Event
	action string
}

const knownCounterCount = 22

var knownCounters = [knownCounterCount]counterKey{
	{EventEpochStart, string(EpochKeepPolicy)},
	{EventBlockReady, string(BlockUseFullWindow)},
	{EventTxAdmit, string(LaneSerial)},
	{EventTxAdmit, string(LaneOptimistic)},
	{EventTxReady, string(LaneSerial)},
	{EventTxReady, string(LaneOptimistic)},
	{EventTaskReady, string(SchedulingFIFO)},
	{EventCallEnter, string(CheckpointDisabled)},
	{EventBranch, string(BranchEvaluateActual)},
	{EventBeforeRead, string(AccessExecute)},
	{EventBeforeWrite, string(AccessExecute)},
	{EventReadEstimate, string(WaitNone)},
	{EventReadEstimate, string(WaitLogicalPredecessor)},
	{EventConflict, string(ConflictValidate)},
	{EventValidationPoint, string(ValidationMandatoryFinal)},
	{EventTxEnd, string(ValidationMandatoryFinal)},
	{EventSubtxEnd, string(ValidationMandatoryFinal)},
	{EventValidationFail, string(RecoveryWholeTransaction)},
	{EventReplayStart, string(ReplayReexecute)},
	{EventRetryLimit, string(FallbackSerial)},
	{EventWorkerIdle, string(ResourceSharedPool)},
	{EventQueuePressure, string(ResourceSharedPool)},
}

func NewRecorder(mode TraceMode) Recorder {
	if !ValidTraceMode(mode) {
		mode = TraceDetailed
	}
	return Recorder{mode: mode}
}

func (r *Recorder) Mode() TraceMode {
	mode := r.mode
	if !ValidTraceMode(mode) {
		return TraceDetailed
	}
	return mode
}

func (r *Recorder) Record(record EventRecord) {
	mode := r.mode
	if !ValidTraceMode(mode) {
		mode = TraceDetailed
	}
	if mode == TraceOff {
		return
	}
	if index, ok := knownCounterIndex(record.Event, record.Action); ok {
		r.counters[index].Add(1)
	} else {
		r.unknownMu.Lock()
		if r.unknownCounters == nil {
			r.unknownCounters = make(map[counterKey]ActionCounter)
		}
		incrementCounter(r.unknownCounters, record)
		r.unknownMu.Unlock()
	}
	r.decisionDurationNS.Add(record.DecisionDurationNS)
	if mode == TraceDetailed {
		r.recordsMu.Lock()
		r.records = append(r.records, record)
		r.recordsMu.Unlock()
	}
}

func (r *Recorder) Snapshot() []EventRecord {
	r.recordsMu.Lock()
	records := append([]EventRecord(nil), r.records...)
	r.recordsMu.Unlock()

	sort.SliceStable(records, func(i, j int) bool {
		left, right := records[i], records[j]
		if left.BlockID != right.BlockID {
			return left.BlockID < right.BlockID
		}
		if left.TransactionID == "" && right.TransactionID != "" {
			return true
		}
		if left.TransactionID != "" && right.TransactionID == "" {
			return false
		}
		if left.TxIndex != right.TxIndex {
			return left.TxIndex < right.TxIndex
		}
		if left.Incarnation != right.Incarnation {
			return left.Incarnation < right.Incarnation
		}
		if left.Ordinal != right.Ordinal {
			return left.Ordinal < right.Ordinal
		}
		if eventRank(left.Event) != eventRank(right.Event) {
			return eventRank(left.Event) < eventRank(right.Event)
		}
		if left.TargetID != right.TargetID {
			return left.TargetID < right.TargetID
		}
		return left.Action < right.Action
	})
	return records
}

func (r *Recorder) Summary() ([]ActionCounter, []ActionCounter, uint64) {
	actions := make([]ActionCounter, 0, knownCounterCount)
	for index, descriptor := range knownCounters {
		if count := r.counters[index].Load(); count > 0 {
			actions = append(actions, ActionCounter{Event: descriptor.event, Action: descriptor.action, Count: count})
		}
	}
	r.unknownMu.Lock()
	actions = append(actions, counterSlice(r.unknownCounters)...)
	r.unknownMu.Unlock()
	sortCounters(actions)
	fallbacks := make([]ActionCounter, 0, 1)
	for _, counter := range actions {
		if counter.Event == EventRetryLimit {
			fallbacks = append(fallbacks, counter)
		}
	}
	duration := r.decisionDurationNS.Load()
	return actions, fallbacks, duration
}

func incrementCounter(counters map[counterKey]ActionCounter, record EventRecord) {
	key := counterKey{event: record.Event, action: record.Action}
	counter := counters[key]
	counter.Event = record.Event
	counter.Action = record.Action
	counter.Count++
	counters[key] = counter
}

func counterSlice(counters map[counterKey]ActionCounter) []ActionCounter {
	values := make([]ActionCounter, 0, len(counters))
	for _, counter := range counters {
		values = append(values, counter)
	}
	sortCounters(values)
	return values
}

func sortCounters(values []ActionCounter) {
	sort.Slice(values, func(i, j int) bool {
		if eventRank(values[i].Event) != eventRank(values[j].Event) {
			return eventRank(values[i].Event) < eventRank(values[j].Event)
		}
		return values[i].Action < values[j].Action
	})
}

func knownCounterIndex(event Event, action string) (int, bool) {
	for index, descriptor := range knownCounters {
		if descriptor.event == event && descriptor.action == action {
			return index, true
		}
	}
	return 0, false
}

func eventRank(event Event) int {
	for index, descriptor := range eventRegistry {
		if descriptor.Event == event {
			return index
		}
	}
	return len(eventRegistry)
}
