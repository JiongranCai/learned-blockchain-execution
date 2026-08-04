package fixed

import (
	"github.com/crypto-org-chain/go-block-stm/internal/control"
	"github.com/crypto-org-chain/go-block-stm/internal/policy"
)

const PresetVersion = "fixed-preset-v1"

type Preset struct {
	identity policy.Identity
	lane     control.AdmissionLane
	wait     control.WaitMode
}

var _ policy.Policy = (*Preset)(nil)

func NewSerialPreset() *Preset {
	return &Preset{
		identity: policy.Identity{
			Name:               "SerialPreset",
			Version:            PresetVersion,
			FeatureSource:      "fixed_config",
			ObservationVersion: "none",
			TableVersion:       PresetVersion,
		},
		lane: control.LaneSerial,
		wait: control.WaitNone,
	}
}

func NewBlockSTMPreset() *Preset {
	return &Preset{
		identity: policy.Identity{
			Name:               "BlockSTMPreset",
			Version:            PresetVersion,
			FeatureSource:      "fixed_config",
			ObservationVersion: "none",
			TableVersion:       PresetVersion,
		},
		lane: control.LaneOptimistic,
		wait: control.WaitLogicalPredecessor,
	}
}

func (p *Preset) Identity() policy.Identity {
	return p.identity
}

func (p *Preset) OnEpochStart(control.EpochContext) control.EpochDecision {
	return control.EpochDecision{Action: control.EpochKeepPolicy}
}

func (p *Preset) OnBlockReady(control.BlockContext) control.BlockDecision {
	return control.BlockDecision{Action: control.BlockUseFullWindow}
}

func (p *Preset) OnTxAdmit(control.TxContext) control.AdmissionDecision {
	return control.AdmissionDecision{Lane: p.lane}
}

func (p *Preset) OnTxReady(control.TxContext) control.AdmissionDecision {
	return control.AdmissionDecision{Lane: p.lane}
}

func (p *Preset) OnEstimateRead(control.ConflictContext) control.WaitDecision {
	return control.WaitDecision{Mode: p.wait}
}

func (p *Preset) OnConflict(control.ConflictContext) control.ConflictDecision {
	return control.ConflictDecision{Mode: control.ConflictValidate}
}

func (p *Preset) OnTaskReady(control.TaskContext) control.SchedulingDecision {
	return control.SchedulingDecision{Mode: control.SchedulingFIFO}
}

func (p *Preset) OnRetryLimit(control.RetryContext) control.FallbackDecision {
	return control.FallbackDecision{Mode: control.FallbackSerial}
}

func (p *Preset) OnWorkerIdle(control.ResourceContext) control.ResourceDecision {
	return control.ResourceDecision{Mode: control.ResourceSharedPool}
}

func (p *Preset) OnQueuePressure(control.ResourceContext) control.ResourceDecision {
	return control.ResourceDecision{Mode: control.ResourceSharedPool}
}

func (p *Preset) BeforeRead(control.AccessContext) control.AccessDecision {
	return control.AccessDecision{Mode: control.AccessExecute}
}

func (p *Preset) BeforeWrite(control.AccessContext) control.AccessDecision {
	return control.AccessDecision{Mode: control.AccessExecute}
}

func (p *Preset) OnBranch(control.BranchContext) control.BranchDecision {
	return control.BranchDecision{Mode: control.BranchEvaluateActual}
}

func (p *Preset) OnCallEnter(control.CallContext) control.CheckpointDecision {
	return control.CheckpointDecision{Mode: control.CheckpointDisabled}
}

func (p *Preset) OnValidationPoint(control.ValidationContext) control.ValidationDecision {
	return control.ValidationDecision{Mode: control.ValidationMandatoryFinal}
}

func (p *Preset) OnValidationFail(control.FailureContext) control.RecoveryDecision {
	return control.RecoveryDecision{Scope: control.RecoveryWholeTransaction}
}

func (p *Preset) OnReplayStart(control.ReplayContext) control.ReplayDecision {
	return control.ReplayDecision{Mode: control.ReplayReexecute}
}
