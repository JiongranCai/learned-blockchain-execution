package engine

import (
	"context"
	"errors"
	"fmt"
	"reflect"

	"github.com/crypto-org-chain/go-block-stm/internal/control"
	"github.com/crypto-org-chain/go-block-stm/internal/model"
	"github.com/crypto-org-chain/go-block-stm/internal/policy"
	"github.com/crypto-org-chain/go-block-stm/internal/state"
)

var (
	ErrNilState                = errors.New("engine requires a non-nil state")
	ErrMissingBlockID          = errors.New("block id is required")
	ErrInvalidWorkers          = errors.New("invalid executor count")
	ErrInvalidSpeculationLimit = errors.New("invalid max speculative inflight")
	ErrInvalidDependencyMode   = errors.New("invalid dependency control")
	ErrInvalidKernelPolicy     = errors.New("invalid kernel execution policy")
	ErrUnsupported             = errors.New("policy decision is unsupported by engine")
)

type RunConfig struct {
	Executors int
	EpochID   string
	Policy    policy.Policy
	TraceMode control.TraceMode
	// MaxSpeculativeInflight bounds admitted transactions beyond the stable
	// validated frontier. Zero means the full block window (W).
	MaxSpeculativeInflight int
	// DependencyMode retains the legacy consumer bundle. The remaining fields
	// expose acquisition, representation, and consumer stages independently.
	// Zero values preserve the runtime-MVCC baseline for direct engine callers.
	DependencyMode                  control.DependencyMode
	DependencySource                control.DependencySource
	DependencyRepresentation        control.DependencyRepresentation
	DependencyRepresentationBuilder control.DependencyRepresentationBuilder
	DependencyWaitPolicy            control.DependencyWaitPolicy
	DependencyEstimateInjection     control.DependencyEstimateInjection
	// DependencyDispatch selects whether a static dependency representation is
	// consumed by an in-callback wait (frozen behaviour) or by deferring the
	// dispatch until the transaction is ready.
	DependencyDispatch control.DependencyDispatchPolicy
	// EstimateReadPolicy and IdleWaitPolicy expose the two frozen Block-STM
	// blocking behaviours. Zero values reproduce the frozen kernel exactly.
	EstimateReadPolicy control.EstimateReadPolicy
	IdleWaitPolicy     control.IdleWaitPolicy
	OmitResultDigest   bool
}

// KernelPlan is the resolved Block-STM kernel policy for a run.
type KernelPlan struct {
	EstimateRead control.EstimateReadPolicy
	IdleWait     control.IdleWaitPolicy
	Dispatch     control.DependencyDispatchPolicy
}

// IsFrozenDefault reports whether the plan reproduces the frozen upstream
// kernel, in which case the engine uses the untouched upstream entry point.
func (p KernelPlan) IsFrozenDefault() bool {
	return p.EstimateRead == control.EstimateReadSuspendInPlace &&
		p.IdleWait == control.IdleWaitGosched &&
		p.Dispatch == control.DependencyDispatchIndexOrder
}

// EffectiveKernelControl resolves and validates the kernel policy. Ready-queue
// dispatch needs a wait consumer to describe which transactions are not ready.
// Defaults do not depend on the speculation window.
func EffectiveKernelControl(config RunConfig, dependency DependencyPlan) (KernelPlan, error) {
	// An omitted field resolves to the behaviour that matches what a dependency
	// DAG actually describes: do not dispatch a transaction that is not ready,
	// and free the worker of a transaction that cannot proceed. Explicit
	// fields are never downgraded.
	waitConsumer := dependency.WaitPolicy != control.DependencyWaitNone

	estimateRead := config.EstimateReadPolicy
	if estimateRead == "" {
		estimateRead = control.EstimateReadAbortReschedule
	}
	if !control.ValidEstimateReadPolicy(estimateRead) {
		return KernelPlan{}, fmt.Errorf("%w: unknown estimate read policy %q", ErrInvalidKernelPolicy, estimateRead)
	}

	dispatch := config.DependencyDispatch
	if dispatch == "" {
		dispatch = control.DependencyDispatchIndexOrder
		if waitConsumer {
			dispatch = control.DependencyDispatchReadyQueue
		}
	}
	if !control.ValidDependencyDispatchPolicy(dispatch) {
		return KernelPlan{}, fmt.Errorf("%w: unknown dispatch policy %q", ErrInvalidKernelPolicy, dispatch)
	}
	if dispatch == control.DependencyDispatchReadyQueue && !waitConsumer {
		return KernelPlan{}, fmt.Errorf("%w: ready_queue dispatch requires a dependency wait consumer", ErrInvalidKernelPolicy)
	}

	idleWait := config.IdleWaitPolicy
	if idleWait == "" {
		idleWait = control.IdleWaitPark
	}
	if !control.ValidIdleWaitPolicy(idleWait) {
		return KernelPlan{}, fmt.Errorf("%w: unknown idle wait policy %q", ErrInvalidKernelPolicy, idleWait)
	}
	if estimateRead == control.EstimateReadSuspendYieldWorker && idleWait != control.IdleWaitPark {
		// Yielding a worker is pointless while freed workers busy-wait. An
		// explicit value is refused rather than rewritten.
		return KernelPlan{}, fmt.Errorf("%w: suspend_yield_worker requires idle_wait=park", ErrInvalidKernelPolicy)
	}

	return KernelPlan{EstimateRead: estimateRead, IdleWait: idleWait, Dispatch: dispatch}, nil
}

type DependencyPlan struct {
	Mode                  control.DependencyMode
	Source                control.DependencySource
	Representation        control.DependencyRepresentation
	RepresentationBuilder control.DependencyRepresentationBuilder
	WaitPolicy            control.DependencyWaitPolicy
	EstimateInjection     control.DependencyEstimateInjection
}

func EffectiveDependencyControl(config RunConfig) (DependencyPlan, error) {
	mode := config.DependencyMode
	if mode == "" {
		mode = control.DependencyMVCCRuntime
	}
	if !control.ValidDependencyMode(mode) {
		return DependencyPlan{}, fmt.Errorf("%w: unknown mode %q", ErrInvalidDependencyMode, mode)
	}
	source := config.DependencySource
	if source == "" {
		if mode != control.DependencyMVCCRuntime {
			return DependencyPlan{}, fmt.Errorf("%w: mode %q requires explicit information source", ErrInvalidDependencyMode, mode)
		}
		source = control.DependencySourceRuntimeObserved
	}
	if !control.ValidDependencySource(source) {
		return DependencyPlan{}, fmt.Errorf("%w: unknown information source %q", ErrInvalidDependencyMode, source)
	}
	if mode != control.DependencyMVCCRuntime && source != control.DependencySourceStaticProgram {
		return DependencyPlan{}, fmt.Errorf("%w: mode %q requires %q information", ErrInvalidDependencyMode, mode, control.DependencySourceStaticProgram)
	}

	representation := config.DependencyRepresentation
	if representation == "" {
		representation = legacyRepresentation(mode)
	}
	if !control.ValidDependencyRepresentation(representation) {
		return DependencyPlan{}, fmt.Errorf("%w: unknown representation %q", ErrInvalidDependencyMode, representation)
	}
	builder := config.DependencyRepresentationBuilder
	if builder == "" {
		builder = defaultRepresentationBuilder(representation)
	}
	if !control.ValidDependencyRepresentationBuilder(builder) {
		return DependencyPlan{}, fmt.Errorf("%w: unknown representation builder %q", ErrInvalidDependencyMode, builder)
	}
	if err := validateRepresentationBuilder(representation, builder); err != nil {
		return DependencyPlan{}, err
	}
	if source == control.DependencySourceRuntimeObserved && representation != control.DependencyRepresentationVersionOnly {
		return DependencyPlan{}, fmt.Errorf("%w: representation %q requires static_program information", ErrInvalidDependencyMode, representation)
	}
	if mode != control.DependencyMVCCRuntime && representation != legacyRepresentation(mode) {
		return DependencyPlan{}, fmt.Errorf("%w: legacy mode %q requires representation %q", ErrInvalidDependencyMode, mode, legacyRepresentation(mode))
	}
	waitPolicy := config.DependencyWaitPolicy
	if waitPolicy == "" {
		waitPolicy = legacyWaitPolicy(mode)
	}
	if !control.ValidDependencyWaitPolicy(waitPolicy) {
		return DependencyPlan{}, fmt.Errorf("%w: unknown wait policy %q", ErrInvalidDependencyMode, waitPolicy)
	}
	estimateInjection := config.DependencyEstimateInjection
	if estimateInjection == "" {
		estimateInjection = legacyEstimateInjection(mode)
	}
	if !control.ValidDependencyEstimateInjection(estimateInjection) {
		return DependencyPlan{}, fmt.Errorf("%w: unknown estimate injection %q", ErrInvalidDependencyMode, estimateInjection)
	}
	if err := validateDependencyConsumers(source, representation, waitPolicy, estimateInjection); err != nil {
		return DependencyPlan{}, err
	}
	if mode != control.DependencyMVCCRuntime &&
		(waitPolicy != legacyWaitPolicy(mode) || estimateInjection != legacyEstimateInjection(mode)) {
		return DependencyPlan{}, fmt.Errorf("%w: legacy mode %q requires wait %q and estimates %q", ErrInvalidDependencyMode, mode, legacyWaitPolicy(mode), legacyEstimateInjection(mode))
	}
	return DependencyPlan{
		Mode:                  mode,
		Source:                source,
		Representation:        representation,
		RepresentationBuilder: builder,
		WaitPolicy:            waitPolicy,
		EstimateInjection:     estimateInjection,
	}, nil
}

func legacyWaitPolicy(mode control.DependencyMode) control.DependencyWaitPolicy {
	switch mode {
	case control.DependencyDeclaredDAG:
		return control.DependencyWaitDirectPredecessors
	case control.DependencySummary:
		return control.DependencyWaitContiguousFrontier
	case control.DependencyFullGraph:
		return control.DependencyWaitAllPredecessors
	default:
		return control.DependencyWaitNone
	}
}

func legacyEstimateInjection(mode control.DependencyMode) control.DependencyEstimateInjection {
	if mode == control.DependencyMVCCRuntime {
		return control.DependencyEstimatesDisabled
	}
	return control.DependencyEstimatesWrite
}

func validateDependencyConsumers(
	source control.DependencySource,
	representation control.DependencyRepresentation,
	waitPolicy control.DependencyWaitPolicy,
	estimateInjection control.DependencyEstimateInjection,
) error {
	if source == control.DependencySourceRuntimeObserved &&
		(waitPolicy != control.DependencyWaitNone || estimateInjection != control.DependencyEstimatesDisabled) {
		return fmt.Errorf("%w: static consumers require static_program information", ErrInvalidDependencyMode)
	}
	wantRepresentation := control.DependencyRepresentationVersionOnly
	switch waitPolicy {
	case control.DependencyWaitDirectPredecessors:
		wantRepresentation = control.DependencyRepresentationRAWLastWriter
	case control.DependencyWaitContiguousFrontier:
		wantRepresentation = control.DependencyRepresentationMaxRAWPredecessor
	case control.DependencyWaitAllPredecessors:
		wantRepresentation = control.DependencyRepresentationFullConflictGraph
	}
	if waitPolicy != control.DependencyWaitNone && representation != wantRepresentation {
		return fmt.Errorf("%w: wait policy %q requires representation %q", ErrInvalidDependencyMode, waitPolicy, wantRepresentation)
	}
	return nil
}

func legacyRepresentation(mode control.DependencyMode) control.DependencyRepresentation {
	switch mode {
	case control.DependencyDeclaredDAG:
		return control.DependencyRepresentationRAWLastWriter
	case control.DependencySummary:
		return control.DependencyRepresentationMaxRAWPredecessor
	case control.DependencyFullGraph:
		return control.DependencyRepresentationFullConflictGraph
	default:
		return control.DependencyRepresentationVersionOnly
	}
}

func defaultRepresentationBuilder(representation control.DependencyRepresentation) control.DependencyRepresentationBuilder {
	switch representation {
	case control.DependencyRepresentationVersionOnly:
		return control.DependencyRepresentationBuilderNone
	case control.DependencyRepresentationFullConflictGraph:
		return control.DependencyRepresentationBuilderQuadraticReference
	default:
		return control.DependencyRepresentationBuilderIndexedByKey
	}
}

func validateRepresentationBuilder(
	representation control.DependencyRepresentation,
	builder control.DependencyRepresentationBuilder,
) error {
	valid := false
	switch representation {
	case control.DependencyRepresentationVersionOnly:
		valid = builder == control.DependencyRepresentationBuilderNone
	case control.DependencyRepresentationRAWLastWriter, control.DependencyRepresentationMaxRAWPredecessor:
		valid = builder == control.DependencyRepresentationBuilderIndexedByKey
	case control.DependencyRepresentationFullConflictGraph:
		valid = builder == control.DependencyRepresentationBuilderIndexedByKey ||
			builder == control.DependencyRepresentationBuilderQuadraticReference
	}
	if !valid {
		return fmt.Errorf("%w: builder %q is invalid for representation %q", ErrInvalidDependencyMode, builder, representation)
	}
	return nil
}

func EffectiveTraceMode(config RunConfig) (control.TraceMode, error) {
	if config.TraceMode == "" {
		return control.TraceDetailed, nil
	}
	if !control.ValidTraceMode(config.TraceMode) {
		return "", fmt.Errorf("invalid trace mode %q", config.TraceMode)
	}
	return config.TraceMode, nil
}

type Engine interface {
	Name() string
	Capabilities() control.Capabilities
	ExecuteBlock(context.Context, model.Block, state.Store, RunConfig) (model.BlockResult, control.Trace, error)
}

func ValidateBlock(block model.Block, storage state.Store) error {
	if storage == nil || isNil(storage) {
		return ErrNilState
	}
	if block.ID == "" {
		return ErrMissingBlockID
	}
	seen := make(map[string]struct{}, len(block.Transactions))
	for index, transaction := range block.Transactions {
		if transaction.ID == "" {
			return fmt.Errorf("transaction %d has no id", index)
		}
		if _, exists := seen[transaction.ID]; exists {
			return fmt.Errorf("duplicate transaction id %q", transaction.ID)
		}
		seen[transaction.ID] = struct{}{}
	}
	return nil
}

func isNil(value any) bool {
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

func EpochID(config RunConfig, block model.Block) string {
	if config.EpochID != "" {
		return config.EpochID
	}
	return block.ID
}

func Unsupported(event control.Event, action string) error {
	return fmt.Errorf("%w: event %s action %q", ErrUnsupported, event, action)
}
