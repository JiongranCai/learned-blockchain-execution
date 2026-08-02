package telemetry

import (
	"fmt"
	"math"
	"sort"

	"github.com/crypto-org-chain/go-block-stm/internal/control"
	"github.com/crypto-org-chain/go-block-stm/internal/model"
)

const (
	BenchmarkRecordSchema  = "benchmark-run-v2"
	ValidationRecordSchema = "validation-run-v2"
	ActionTraceSchema      = "action-trace-v2"
	AblationRecordSchema   = "telemetry-ablation-v1"
)

type Case struct {
	ID                     string            `json:"id"`
	Engine                 string            `json:"engine"`
	Policy                 string            `json:"policy"`
	PolicyVersion          string            `json:"policy_version"`
	Executors              int               `json:"executors"`
	MaxSpeculativeInflight int               `json:"max_speculative_inflight"`
	TraceMode              control.TraceMode `json:"trace_mode"`
}

type Environment struct {
	Affinity     string `json:"affinity"`
	NUMAPolicy   string `json:"numa_policy"`
	StateReset   string `json:"state_reset"`
	PageCache    string `json:"page_cache"`
	ProcessReuse string `json:"process_reuse"`
}

type Provenance struct {
	CodeCommit               string      `json:"code_commit"`
	CodeModified             bool        `json:"code_modified"`
	BinarySHA256             string      `json:"binary_sha256"`
	ProcessID                int         `json:"process_id"`
	UpstreamCommit           string      `json:"upstream_commit"`
	ConfigSchemaVersion      string      `json:"config_schema_version"`
	ConfigSchemaHash         string      `json:"config_schema_hash"`
	ConfigHash               string      `json:"config_hash"`
	StatisticalSchemaVersion string      `json:"statistical_schema_version"`
	StatisticalSchemaHash    string      `json:"statistical_schema_hash"`
	StatisticalProtocolHash  string      `json:"statistical_protocol_hash"`
	WorkloadSchemaVersion    string      `json:"workload_schema_version"`
	WorkloadHash             string      `json:"workload_hash"`
	GeneratorVersion         string      `json:"generator_version"`
	GeneratorSeed            int64       `json:"generator_seed"`
	Hardware                 Hardware    `json:"hardware"`
	Environment              Environment `json:"environment"`
	RunProtocol              RunProtocol `json:"run_protocol"`
}

type RunProtocol struct {
	RunClass          string `json:"run_class"`
	WarmupRounds      int    `json:"warmup_rounds"`
	MeasurementRounds int    `json:"measurement_rounds"`
	OrderSeed         int64  `json:"order_seed"`
	Timeout           string `json:"timeout"`
}

type Hardware struct {
	Hostname          string `json:"hostname"`
	GOOS              string `json:"goos"`
	GOARCH            string `json:"goarch"`
	CPUModel          string `json:"cpu_model"`
	LogicalCPUs       int    `json:"logical_cpus"`
	MemoryBytes       uint64 `json:"memory_bytes"`
	Kernel            string `json:"kernel"`
	GoVersion         string `json:"go_version"`
	GOMAXPROCS        int    `json:"gomaxprocs"`
	CPUAllowedList    string `json:"cpu_allowed_list"`
	MemoryAllowedList string `json:"memory_allowed_list"`
	CPUGovernor       string `json:"cpu_governor"`
}

type Timing struct {
	ExecutionNS    uint64   `json:"execution_ns"`
	BlockLatencyNS []uint64 `json:"block_latency_ns"`
}

type Metrics struct {
	Blocks                        uint64                  `json:"blocks"`
	Transactions                  uint64                  `json:"transactions"`
	SuccessfulTransactions        uint64                  `json:"successful_transactions"`
	FailedTransactions            uint64                  `json:"failed_transactions"`
	UsefulExecutionUnits          uint64                  `json:"useful_execution_units"`
	ReexecutedExecutionUnits      uint64                  `json:"reexecuted_execution_units"`
	DiscardedExecutionUnits       uint64                  `json:"discarded_execution_units"`
	ExecutionAttempts             uint64                  `json:"execution_attempts"`
	ReexecutionAttempts           uint64                  `json:"reexecution_attempts"`
	CompletedTransactionsPerS     float64                 `json:"completed_transactions_per_second"`
	CommittedGoodputPerS          float64                 `json:"committed_goodput_per_second"`
	ValidationEvents              uint64                  `json:"validation_events"`
	ValidationFailures            uint64                  `json:"validation_failures"`
	ReexecutionEvents             uint64                  `json:"reexecution_events"`
	WaitEvents                    uint64                  `json:"wait_events"`
	WorkerIdleEvents              uint64                  `json:"worker_idle_events"`
	QueuePressureEvents           uint64                  `json:"queue_pressure_events"`
	PolicyDecisionNS              uint64                  `json:"policy_decision_ns"`
	MaxRSSBytes                   uint64                  `json:"max_rss_bytes"`
	ActionCounters                []control.ActionCounter `json:"action_counters,omitempty"`
	FallbackCounters              []control.ActionCounter `json:"fallback_counters,omitempty"`
	EffectiveSpeculationLimit     uint64                  `json:"effective_speculation_limit"`
	SpeculationLimitApplied       bool                    `json:"speculation_limit_applied"`
	SpeculationTelemetryAvailable bool                    `json:"speculation_telemetry_available"`
	PeakSpeculativeInflight       uint64                  `json:"peak_speculative_inflight"`
	AdmissionStallEvents          uint64                  `json:"admission_stall_events"`
	AdmissionStallNS              uint64                  `json:"admission_stall_ns"`
	Unavailable                   []string                `json:"unavailable"`
}

type BenchmarkRecord struct {
	SchemaVersion  string               `json:"schema_version"`
	RunID          string               `json:"run_id"`
	Mode           string               `json:"mode"`
	Status         string               `json:"status"`
	Censored       bool                 `json:"censored"`
	Error          string               `json:"error,omitempty"`
	Phase          string               `json:"phase"`
	Round          int                  `json:"round"`
	Order          int                  `json:"order"`
	Case           Case                 `json:"case"`
	Capabilities   control.Capabilities `json:"capabilities"`
	Provenance     Provenance           `json:"provenance"`
	Timing         Timing               `json:"timing"`
	Metrics        Metrics              `json:"metrics"`
	BlockDigests   []string             `json:"block_digests"`
	ResultDigest   string               `json:"result_digest"`
	CanonicalMatch bool                 `json:"canonical_match"`
}

type ValidationRecord struct {
	SchemaVersion         string               `json:"schema_version"`
	RunID                 string               `json:"run_id"`
	Status                string               `json:"status"`
	Error                 string               `json:"error,omitempty"`
	Case                  Case                 `json:"case"`
	Capabilities          control.Capabilities `json:"capabilities"`
	Provenance            Provenance           `json:"provenance"`
	OracleResultDigest    string               `json:"oracle_result_digest"`
	CandidateResultDigest string               `json:"candidate_result_digest"`
	CanonicalMatch        bool                 `json:"canonical_match"`
}

type ActionTraceRecord struct {
	SchemaVersion string        `json:"schema_version"`
	RunID         string        `json:"run_id"`
	BlockID       string        `json:"block_id"`
	Trace         control.Trace `json:"trace"`
}

type AblationRecord struct {
	SchemaVersion        string  `json:"schema_version"`
	OffCase              string  `json:"off_case"`
	InstrumentedCase     string  `json:"instrumented_case"`
	OffMedianNS          uint64  `json:"off_median_ns"`
	InstrumentedMedianNS uint64  `json:"instrumented_median_ns"`
	OverheadRatio        float64 `json:"overhead_ratio"`
	BudgetRatio          float64 `json:"budget_ratio"`
	Enforced             bool    `json:"enforced"`
	WithinBudget         bool    `json:"within_budget"`
}

func CollectMetrics(results []model.BlockResult, traces []control.Trace, executionNS uint64, maxRSS uint64) Metrics {
	metrics := Metrics{
		Blocks:      uint64(len(results)),
		MaxRSSBytes: maxRSS,
		Unavailable: []string{
			"transaction_latency: engine does not expose per-transaction timestamps",
			"wait_time: frozen Block-STM callback is unavailable",
			"worker_idle_time: frozen scheduler callback is unavailable",
			"graph_construction: dependency modes are not implemented in M1",
			"checkpoint_replay: nested recovery is not implemented in M1",
		},
	}
	for _, result := range results {
		for _, transaction := range result.Transactions {
			metrics.Transactions++
			metrics.UsefulExecutionUnits += transaction.UnitsUsed
			if transaction.Status == model.TxStatusSuccess {
				metrics.SuccessfulTransactions++
			} else {
				metrics.FailedTransactions++
			}
		}
	}
	if executionNS > 0 {
		seconds := float64(executionNS) / 1e9
		metrics.CompletedTransactionsPerS = float64(metrics.Transactions) / seconds
		metrics.CommittedGoodputPerS = float64(metrics.SuccessfulTransactions) / seconds
	}
	actions := make(map[string]control.ActionCounter)
	fallbacks := make(map[string]control.ActionCounter)
	detailedTraceSeen := false
	workTelemetrySeen := false
	speculationTelemetrySeen := false
	for _, trace := range traces {
		if trace.Mode == control.TraceDetailed {
			detailedTraceSeen = true
		}
		metrics.PolicyDecisionNS += trace.PolicyDecisionDurationNS
		if trace.WorkAvailable {
			workTelemetrySeen = true
			if trace.Work.SpeculationLimit > metrics.EffectiveSpeculationLimit {
				metrics.EffectiveSpeculationLimit = trace.Work.SpeculationLimit
			}
			metrics.SpeculationLimitApplied = metrics.SpeculationLimitApplied || trace.Work.SpeculationLimitApplied
			if trace.Work.SpeculationTelemetryAvailable {
				speculationTelemetrySeen = true
				metrics.SpeculationTelemetryAvailable = true
			}
			if trace.Work.PeakSpeculativeInflight > metrics.PeakSpeculativeInflight {
				metrics.PeakSpeculativeInflight = trace.Work.PeakSpeculativeInflight
			}
			metrics.AdmissionStallEvents += trace.Work.AdmissionStallEvents
			metrics.AdmissionStallNS += trace.Work.AdmissionStallNS
			metrics.ExecutionAttempts += trace.Work.ExecutionAttempts
			metrics.ReexecutionAttempts += trace.Work.ReexecutionAttempts
			metrics.ReexecutedExecutionUnits += trace.Work.ReexecutedExecutionUnits
			metrics.DiscardedExecutionUnits += trace.Work.DiscardedExecutionUnits
		}
		for _, counter := range trace.ActionCounters {
			addCounter(actions, counter)
			switch counter.Event {
			case control.EventValidationPoint, control.EventTxEnd, control.EventSubtxEnd:
				metrics.ValidationEvents += counter.Count
			case control.EventValidationFail:
				metrics.ValidationFailures += counter.Count
			case control.EventReplayStart:
				metrics.ReexecutionEvents += counter.Count
			case control.EventReadEstimate:
				metrics.WaitEvents += counter.Count
			case control.EventWorkerIdle:
				metrics.WorkerIdleEvents += counter.Count
			case control.EventQueuePressure:
				metrics.QueuePressureEvents += counter.Count
			}
		}
		for _, counter := range trace.FallbackCounters {
			addCounter(fallbacks, counter)
		}
	}
	metrics.ActionCounters = sortedCounters(actions)
	metrics.FallbackCounters = sortedCounters(fallbacks)
	if !detailedTraceSeen {
		metrics.Unavailable = append(metrics.Unavailable, "policy_decision_time: detailed trace mode is disabled")
	}
	if !workTelemetrySeen {
		metrics.Unavailable = append(metrics.Unavailable, "reexecution_work: telemetry is disabled")
	}
	if workTelemetrySeen && !speculationTelemetrySeen {
		metrics.Unavailable = append(metrics.Unavailable, "peak_speculative_inflight: original full-window path does not expose exact admission occupancy")
	}
	return metrics
}

func NewAblationRecord(offCase, instrumentedCase string, off, instrumented []uint64, budget float64, enforced bool) (AblationRecord, error) {
	if len(off) == 0 || len(instrumented) == 0 {
		return AblationRecord{}, fmt.Errorf("telemetry ablation requires measurement samples for both cases")
	}
	offMedian := median(off)
	instrumentedMedian := median(instrumented)
	overhead := 0.0
	if offMedian > 0 {
		overhead = float64(instrumentedMedian)/float64(offMedian) - 1
	}
	return AblationRecord{
		SchemaVersion:        AblationRecordSchema,
		OffCase:              offCase,
		InstrumentedCase:     instrumentedCase,
		OffMedianNS:          offMedian,
		InstrumentedMedianNS: instrumentedMedian,
		OverheadRatio:        overhead,
		BudgetRatio:          budget,
		Enforced:             enforced,
		WithinBudget:         overhead <= budget || math.Abs(overhead-budget) < 1e-12,
	}, nil
}

func addCounter(target map[string]control.ActionCounter, counter control.ActionCounter) {
	key := string(counter.Event) + "\x00" + counter.Action
	current := target[key]
	current.Event = counter.Event
	current.Action = counter.Action
	current.Count += counter.Count
	target[key] = current
}

func sortedCounters(values map[string]control.ActionCounter) []control.ActionCounter {
	counters := make([]control.ActionCounter, 0, len(values))
	for _, counter := range values {
		counters = append(counters, counter)
	}
	sort.Slice(counters, func(i, j int) bool {
		if counters[i].Event != counters[j].Event {
			return counters[i].Event < counters[j].Event
		}
		return counters[i].Action < counters[j].Action
	})
	return counters
}

func median(values []uint64) uint64 {
	ordered := append([]uint64(nil), values...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i] < ordered[j] })
	middle := len(ordered) / 2
	if len(ordered)%2 == 1 {
		return ordered[middle]
	}
	return ordered[middle-1]/2 + ordered[middle]/2 + (ordered[middle-1]%2+ordered[middle]%2)/2
}
