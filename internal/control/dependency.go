package control

// DependencyMode selects how pre-execution dependency information is
// represented and used. MVCCRuntime is the B0 path and performs no static
// dependency planning.
type DependencyMode string

const (
	DependencyMVCCRuntime DependencyMode = "mvcc_runtime"
	DependencyDeclaredDAG DependencyMode = "declared_dag"
	DependencySummary     DependencyMode = "summary"
	DependencyFullGraph   DependencyMode = "full_graph"
)

func ValidDependencyMode(mode DependencyMode) bool {
	switch mode {
	case DependencyMVCCRuntime, DependencyDeclaredDAG, DependencySummary, DependencyFullGraph:
		return true
	default:
		return false
	}
}

// DependencySource identifies the information-acquisition path. Static
// program information is obtained by scanning the engine-visible transaction
// programs during the timed ExecuteBlock call; it never reads workload ground
// truth. MVCC runtime information is discovered by the frozen kernel.
type DependencySource string

const (
	DependencySourceRuntimeObserved DependencySource = "runtime_observed"
	DependencySourceStaticProgram   DependencySource = "static_program"
)

func ValidDependencySource(source DependencySource) bool {
	switch source {
	case DependencySourceRuntimeObserved, DependencySourceStaticProgram:
		return true
	default:
		return false
	}
}

const (
	DependencyAvailableDuringExecution  = "during_execution"
	DependencyAvailableBeforeExecution  = "before_execution"
	DependencySourceVersionRuntimeMVCC  = "blockstm-mvcc"
	DependencySourceVersionStaticScan   = "flat-static-scan-v1"
	DependencyDispositionRuntimeKernel  = "runtime_kernel"
	DependencyDispositionDiscarded      = "discarded_after_acquisition"
	DependencyDispositionRepresentation = "representation_input"
)

// DependencyCounters separates dependency information acquisition, representation,
// and scheduling use. LogicalBytes is a deterministic representation-size
// accounting value, not a claim about Go heap allocation. AcquisitionNS and
// RepresentationNS are sequential block-planning wall time; ResolutionNS and
// WaitNS sum time across transaction callbacks and may exceed block wall time.
type DependencyCounters struct {
	Mode                         DependencyMode   `json:"mode"`
	Source                       DependencySource `json:"source"`
	SourceAvailableAt            string           `json:"source_available_at"`
	SourceVersion                string           `json:"source_version"`
	AcquisitionDisposition       string           `json:"acquisition_disposition"`
	InformationComplete          bool             `json:"information_complete"`
	InformationExact             bool             `json:"information_exact"`
	AcquisitionMeasured          bool             `json:"acquisition_measured"`
	AcquisitionNS                uint64           `json:"acquisition_ns"`
	AcquisitionUnits             uint64           `json:"acquisition_units"`
	AcquisitionBytes             uint64           `json:"acquisition_bytes"`
	StaticReadKeys               uint64           `json:"static_read_keys"`
	StaticWriteKeys              uint64           `json:"static_write_keys"`
	RepresentationMeasured       bool             `json:"representation_measured"`
	RepresentationNS             uint64           `json:"representation_ns"`
	RepresentationLogicalBytes   uint64           `json:"representation_logical_bytes"`
	DependencyEdges              uint64           `json:"dependency_edges"`
	SummaryEntries               uint64           `json:"summary_entries"`
	EstimatedWriteLocations      uint64           `json:"estimated_write_locations"`
	EstimatedWriteKeyBytes       uint64           `json:"estimated_write_key_bytes"`
	ResolutionMeasured           bool             `json:"resolution_measured"`
	ResolutionNS                 uint64           `json:"resolution_ns"`
	PlanLookups                  uint64           `json:"plan_lookups"`
	TraversalSteps               uint64           `json:"traversal_steps"`
	WaitedExecutionAttempts      uint64           `json:"waited_execution_attempts"`
	WaitEvents                   uint64           `json:"wait_events"`
	WaitNS                       uint64           `json:"wait_ns"`
	PostGuidanceReexecutions     uint64           `json:"post_guidance_reexecution_attempts"`
	PostGuidanceReexecutionUnits uint64           `json:"post_guidance_reexecution_units"`
}
