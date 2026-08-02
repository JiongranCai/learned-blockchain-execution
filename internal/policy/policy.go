package policy

import "github.com/crypto-org-chain/go-block-stm/internal/control"

type Identity struct {
	Name               string `json:"name"`
	Version            string `json:"version"`
	FeatureSource      string `json:"feature_source,omitempty"`
	ObservationVersion string `json:"observation_version,omitempty"`
	TableVersion       string `json:"table_version,omitempty"`
}

type MacroPolicy interface {
	OnEpochStart(control.EpochContext) control.EpochDecision
	OnBlockReady(control.BlockContext) control.BlockDecision
}

type TxPolicy interface {
	OnTxAdmit(control.TxContext) control.AdmissionDecision
	OnTxReady(control.TxContext) control.AdmissionDecision
	OnEstimateRead(control.ConflictContext) control.WaitDecision
	OnConflict(control.ConflictContext) control.ConflictDecision
	OnTaskReady(control.TaskContext) control.SchedulingDecision
	OnRetryLimit(control.RetryContext) control.FallbackDecision
	OnWorkerIdle(control.ResourceContext) control.ResourceDecision
	OnQueuePressure(control.ResourceContext) control.ResourceDecision
}

type RuntimeHooks interface {
	BeforeRead(control.AccessContext) control.AccessDecision
	BeforeWrite(control.AccessContext) control.AccessDecision
	OnBranch(control.BranchContext) control.BranchDecision
	OnCallEnter(control.CallContext) control.CheckpointDecision
	OnValidationPoint(control.ValidationContext) control.ValidationDecision
}

type RecoveryPolicy interface {
	OnValidationFail(control.FailureContext) control.RecoveryDecision
	OnReplayStart(control.ReplayContext) control.ReplayDecision
}

type Policy interface {
	Identity() Identity
	MacroPolicy
	TxPolicy
	RuntimeHooks
	RecoveryPolicy
}

// RuntimeDefaults provides the only flat-runtime actions available in Week 3.
// Engines use a full fixed Policy; direct runtime callers can use this value.
type RuntimeDefaults struct{}

func (RuntimeDefaults) BeforeRead(control.AccessContext) control.AccessDecision {
	return control.AccessDecision{Mode: control.AccessExecute}
}

func (RuntimeDefaults) BeforeWrite(control.AccessContext) control.AccessDecision {
	return control.AccessDecision{Mode: control.AccessExecute}
}

func (RuntimeDefaults) OnBranch(control.BranchContext) control.BranchDecision {
	return control.BranchDecision{Mode: control.BranchEvaluateActual}
}

func (RuntimeDefaults) OnCallEnter(control.CallContext) control.CheckpointDecision {
	return control.CheckpointDecision{Mode: control.CheckpointDisabled}
}

func (RuntimeDefaults) OnValidationPoint(control.ValidationContext) control.ValidationDecision {
	return control.ValidationDecision{Mode: control.ValidationMandatoryFinal}
}
