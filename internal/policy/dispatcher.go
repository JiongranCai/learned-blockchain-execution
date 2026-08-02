package policy

import (
	"errors"
	"fmt"
	"reflect"
	"sync"
	"time"

	"github.com/crypto-org-chain/go-block-stm/internal/control"
)

var (
	ErrInvalidPolicy   = errors.New("invalid policy")
	ErrInvalidDecision = errors.New("invalid policy decision")
)

// Dispatcher is the single typed policy seam used by engines and runtimes. It
// validates every returned action and records a concurrency-safe logical trace.
type Dispatcher struct {
	policy    Policy
	identity  Identity
	recorder  control.Recorder
	traceMode control.TraceMode

	errMu sync.Mutex
	err   error
}

func NewDispatcher(value Policy) (*Dispatcher, error) {
	return NewDispatcherWithTrace(value, control.TraceDetailed)
}

func NewDispatcherWithTrace(value Policy, traceMode control.TraceMode) (*Dispatcher, error) {
	if value == nil || isNil(value) {
		return nil, fmt.Errorf("%w: policy is nil", ErrInvalidPolicy)
	}
	if !control.ValidTraceMode(traceMode) {
		return nil, fmt.Errorf("%w: invalid trace mode %q", ErrInvalidPolicy, traceMode)
	}
	identity := value.Identity()
	if identity.Name == "" || identity.Version == "" {
		return nil, fmt.Errorf("%w: policy name and version are required", ErrInvalidPolicy)
	}
	if identity.FeatureSource == "" {
		identity.FeatureSource = "unspecified"
	}
	if identity.ObservationVersion == "" {
		identity.ObservationVersion = "unspecified"
	}
	if identity.TableVersion == "" {
		identity.TableVersion = identity.Version
	}
	return &Dispatcher{
		policy:    value,
		identity:  identity,
		recorder:  control.NewRecorder(traceMode),
		traceMode: traceMode,
	}, nil
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

func (d *Dispatcher) Identity() Identity {
	return d.identity
}

func (d *Dispatcher) Err() error {
	d.errMu.Lock()
	err := d.err
	d.errMu.Unlock()
	return err
}

func (d *Dispatcher) Trace(engine string) control.Trace {
	actions, fallbacks, duration := d.recorder.Summary()
	return control.Trace{
		Engine:                   engine,
		PolicyName:               d.identity.Name,
		PolicyVersion:            d.identity.Version,
		Mode:                     d.traceMode,
		Events:                   d.recorder.Snapshot(),
		ActionCounters:           actions,
		FallbackCounters:         fallbacks,
		PolicyDecisionDurationNS: duration,
	}
}

func (d *Dispatcher) OnEpochStart(ctx control.EpochContext) control.EpochDecision {
	started := d.startDecision()
	decision := d.policy.OnEpochStart(ctx)
	d.validate(decision.Action == control.EpochKeepPolicy, control.EventEpochStart, string(decision.Action))
	d.recordMacro(control.EventEpochStart, "", ctx.EpochID, string(decision.Action), started)
	return decision
}

func (d *Dispatcher) OnBlockReady(ctx control.BlockContext) control.BlockDecision {
	started := d.startDecision()
	decision := d.policy.OnBlockReady(ctx)
	d.validate(decision.Action == control.BlockUseFullWindow, control.EventBlockReady, string(decision.Action))
	d.recordMacro(control.EventBlockReady, ctx.BlockID, ctx.BlockID, string(decision.Action), started)
	return decision
}

func (d *Dispatcher) OnTxAdmit(ctx control.TxContext) control.AdmissionDecision {
	started := d.startDecision()
	decision := d.policy.OnTxAdmit(ctx)
	d.validate(validLane(decision.Lane), control.EventTxAdmit, string(decision.Lane))
	d.recordTx(control.EventTxAdmit, ctx, ctx.TransactionID, string(decision.Lane), started)
	return decision
}

func (d *Dispatcher) OnTxReady(ctx control.TxContext) control.AdmissionDecision {
	started := d.startDecision()
	decision := d.policy.OnTxReady(ctx)
	d.validate(validLane(decision.Lane), control.EventTxReady, string(decision.Lane))
	d.recordTx(control.EventTxReady, ctx, ctx.TransactionID, string(decision.Lane), started)
	return decision
}

func (d *Dispatcher) OnEstimateRead(ctx control.ConflictContext) control.WaitDecision {
	started := d.startDecision()
	decision := d.policy.OnEstimateRead(ctx)
	d.validate(validWaitMode(decision.Mode), control.EventReadEstimate, string(decision.Mode))
	d.recordTx(control.EventReadEstimate, ctx.TxContext, ctx.TransactionID, string(decision.Mode), started)
	return decision
}

func (d *Dispatcher) OnConflict(ctx control.ConflictContext) control.ConflictDecision {
	started := d.startDecision()
	decision := d.policy.OnConflict(ctx)
	d.validate(decision.Mode == control.ConflictValidate, control.EventConflict, string(decision.Mode))
	d.recordTx(control.EventConflict, ctx.TxContext, ctx.TransactionID, string(decision.Mode), started)
	return decision
}

func (d *Dispatcher) OnTaskReady(ctx control.TaskContext) control.SchedulingDecision {
	started := d.startDecision()
	decision := d.policy.OnTaskReady(ctx)
	d.validate(decision.Mode == control.SchedulingFIFO, control.EventTaskReady, string(decision.Mode))
	d.recordTx(control.EventTaskReady, ctx.TxContext, ctx.TransactionID, string(decision.Mode), started)
	return decision
}

func (d *Dispatcher) OnRetryLimit(ctx control.RetryContext) control.FallbackDecision {
	started := d.startDecision()
	decision := d.policy.OnRetryLimit(ctx)
	d.validate(decision.Mode == control.FallbackSerial, control.EventRetryLimit, string(decision.Mode))
	d.recordTx(control.EventRetryLimit, ctx.TxContext, ctx.TransactionID, string(decision.Mode), started)
	return decision
}

func (d *Dispatcher) OnWorkerIdle(ctx control.ResourceContext) control.ResourceDecision {
	started := d.startDecision()
	decision := d.policy.OnWorkerIdle(ctx)
	d.validate(decision.Mode == control.ResourceSharedPool, control.EventWorkerIdle, string(decision.Mode))
	d.recordResource(control.EventWorkerIdle, ctx, string(decision.Mode), started)
	return decision
}

func (d *Dispatcher) OnQueuePressure(ctx control.ResourceContext) control.ResourceDecision {
	started := d.startDecision()
	decision := d.policy.OnQueuePressure(ctx)
	d.validate(decision.Mode == control.ResourceSharedPool, control.EventQueuePressure, string(decision.Mode))
	d.recordResource(control.EventQueuePressure, ctx, string(decision.Mode), started)
	return decision
}

func (d *Dispatcher) BeforeRead(ctx control.AccessContext) control.AccessDecision {
	started := d.startDecision()
	decision := d.policy.BeforeRead(ctx)
	d.validate(decision.Mode == control.AccessExecute, control.EventBeforeRead, string(decision.Mode))
	d.recordTx(control.EventBeforeRead, ctx.TxContext, ctx.OperationID, string(decision.Mode), started)
	return decision
}

func (d *Dispatcher) BeforeWrite(ctx control.AccessContext) control.AccessDecision {
	started := d.startDecision()
	decision := d.policy.BeforeWrite(ctx)
	d.validate(decision.Mode == control.AccessExecute, control.EventBeforeWrite, string(decision.Mode))
	d.recordTx(control.EventBeforeWrite, ctx.TxContext, ctx.OperationID, string(decision.Mode), started)
	return decision
}

func (d *Dispatcher) OnBranch(ctx control.BranchContext) control.BranchDecision {
	started := d.startDecision()
	decision := d.policy.OnBranch(ctx)
	d.validate(decision.Mode == control.BranchEvaluateActual, control.EventBranch, string(decision.Mode))
	d.recordTx(control.EventBranch, ctx.TxContext, ctx.BranchID, string(decision.Mode), started)
	return decision
}

func (d *Dispatcher) OnCallEnter(ctx control.CallContext) control.CheckpointDecision {
	started := d.startDecision()
	decision := d.policy.OnCallEnter(ctx)
	d.validate(decision.Mode == control.CheckpointDisabled, control.EventCallEnter, string(decision.Mode))
	d.recordTx(control.EventCallEnter, ctx.TxContext, ctx.CallID, string(decision.Mode), started)
	return decision
}

func (d *Dispatcher) OnValidationPoint(ctx control.ValidationContext) control.ValidationDecision {
	started := d.startDecision()
	decision := d.policy.OnValidationPoint(ctx)
	event, valid := validationEvent(ctx.Kind)
	d.validate(valid && decision.Mode == control.ValidationMandatoryFinal, event, string(decision.Mode))
	d.recordTx(event, ctx.TxContext, ctx.TargetID, string(decision.Mode), started)
	return decision
}

func (d *Dispatcher) OnValidationFail(ctx control.FailureContext) control.RecoveryDecision {
	started := d.startDecision()
	decision := d.policy.OnValidationFail(ctx)
	d.validate(decision.Scope == control.RecoveryWholeTransaction, control.EventValidationFail, string(decision.Scope))
	d.recordTx(control.EventValidationFail, ctx.TxContext, ctx.TransactionID, string(decision.Scope), started)
	return decision
}

func (d *Dispatcher) OnReplayStart(ctx control.ReplayContext) control.ReplayDecision {
	started := d.startDecision()
	decision := d.policy.OnReplayStart(ctx)
	d.validate(decision.Mode == control.ReplayReexecute, control.EventReplayStart, string(decision.Mode))
	d.recordTx(control.EventReplayStart, ctx.TxContext, ctx.TransactionID, string(decision.Mode), started)
	return decision
}

func (d *Dispatcher) validate(valid bool, event control.Event, action string) {
	if valid {
		return
	}
	d.errMu.Lock()
	if d.err == nil {
		d.err = fmt.Errorf("%w: event %s returned action %q", ErrInvalidDecision, event, action)
	}
	d.errMu.Unlock()
}

func (d *Dispatcher) startDecision() time.Time {
	if d.traceMode != control.TraceDetailed {
		return time.Time{}
	}
	return time.Now()
}

func (d *Dispatcher) recordMacro(event control.Event, blockID, targetID, action string, started time.Time) {
	d.record(control.EventRecord{
		Event:    event,
		BlockID:  blockID,
		TargetID: targetID,
		Action:   action,
	}, started)
}

func (d *Dispatcher) recordTx(event control.Event, ctx control.TxContext, targetID, action string, started time.Time) {
	d.record(control.EventRecord{
		Event:         event,
		BlockID:       ctx.BlockID,
		TransactionID: ctx.TransactionID,
		TxIndex:       ctx.TxIndex,
		Incarnation:   ctx.Incarnation,
		Ordinal:       ctx.Ordinal,
		TargetID:      targetID,
		Action:        action,
	}, started)
}

func (d *Dispatcher) recordResource(event control.Event, ctx control.ResourceContext, action string, started time.Time) {
	d.record(control.EventRecord{
		Event:    event,
		BlockID:  ctx.BlockID,
		Ordinal:  ctx.Ordinal,
		TargetID: fmt.Sprintf("worker-%d", ctx.Worker),
		Action:   action,
	}, started)
}

func (d *Dispatcher) record(record control.EventRecord, started time.Time) {
	if d.traceMode == control.TraceOff {
		return
	}
	if d.traceMode == control.TraceCounters {
		d.recorder.Record(control.EventRecord{Event: record.Event, Action: record.Action})
		return
	}
	descriptor, ok := control.EventDescription(record.Event)
	if ok {
		record.TrustClass = descriptor.TrustClass
	}
	record.FeatureSource = d.identity.FeatureSource
	record.ObservationVersion = d.identity.ObservationVersion
	record.PolicyTableVersion = d.identity.TableVersion
	if !started.IsZero() {
		record.DecisionDurationNS = uint64(time.Since(started))
	}
	d.recorder.Record(record)
}

func validLane(lane control.AdmissionLane) bool {
	return lane == control.LaneSerial || lane == control.LaneOptimistic
}

func validWaitMode(mode control.WaitMode) bool {
	return mode == control.WaitNone || mode == control.WaitLogicalPredecessor
}

func validationEvent(kind control.ValidationKind) (control.Event, bool) {
	switch kind {
	case control.ValidationExplicit:
		return control.EventValidationPoint, true
	case control.ValidationTxEnd:
		return control.EventTxEnd, true
	case control.ValidationSubtxEnd:
		return control.EventSubtxEnd, true
	default:
		return control.EventValidationPoint, false
	}
}
