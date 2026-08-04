package fixed_test

import (
	"testing"

	"github.com/crypto-org-chain/go-block-stm/internal/control"
	"github.com/crypto-org-chain/go-block-stm/internal/policy"
	"github.com/crypto-org-chain/go-block-stm/internal/policy/fixed"
)

func TestPresetsProvideExplicitWeekThreeDefaults(t *testing.T) {
	tests := []struct {
		name string
		set  policy.Policy
		lane control.AdmissionLane
		wait control.WaitMode
	}{
		{name: "serial", set: fixed.NewSerialPreset(), lane: control.LaneSerial, wait: control.WaitNone},
		{name: "blockstm", set: fixed.NewBlockSTMPreset(), lane: control.LaneOptimistic, wait: control.WaitLogicalPredecessor},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if identity := test.set.Identity(); identity.Name == "" || identity.Version == "" {
				t.Fatalf("missing immutable identity: %#v", identity)
			}
			if got := test.set.OnEpochStart(control.EpochContext{}).Action; got != control.EpochKeepPolicy {
				t.Fatalf("epoch action: %q", got)
			}
			if got := test.set.OnBlockReady(control.BlockContext{}).Action; got != control.BlockUseFullWindow {
				t.Fatalf("block action: %q", got)
			}
			if got := test.set.OnTxAdmit(control.TxContext{}).Lane; got != test.lane {
				t.Fatalf("admit lane: %q", got)
			}
			if got := test.set.OnTxReady(control.TxContext{}).Lane; got != test.lane {
				t.Fatalf("ready lane: %q", got)
			}
			if got := test.set.OnEstimateRead(control.ConflictContext{}).Mode; got != test.wait {
				t.Fatalf("estimate wait: %q", got)
			}
			if got := test.set.OnConflict(control.ConflictContext{}).Mode; got != control.ConflictValidate {
				t.Fatalf("conflict mode: %q", got)
			}
			if got := test.set.OnTaskReady(control.TaskContext{}).Mode; got != control.SchedulingFIFO {
				t.Fatalf("schedule mode: %q", got)
			}
			if got := test.set.OnRetryLimit(control.RetryContext{}).Mode; got != control.FallbackSerial {
				t.Fatalf("fallback mode: %q", got)
			}
			if got := test.set.OnWorkerIdle(control.ResourceContext{}).Mode; got != control.ResourceSharedPool {
				t.Fatalf("idle resource mode: %q", got)
			}
			if got := test.set.OnQueuePressure(control.ResourceContext{}).Mode; got != control.ResourceSharedPool {
				t.Fatalf("queue resource mode: %q", got)
			}
			if got := test.set.BeforeRead(control.AccessContext{}).Mode; got != control.AccessExecute {
				t.Fatalf("read mode: %q", got)
			}
			if got := test.set.BeforeWrite(control.AccessContext{}).Mode; got != control.AccessExecute {
				t.Fatalf("write mode: %q", got)
			}
			if got := test.set.OnBranch(control.BranchContext{}).Mode; got != control.BranchEvaluateActual {
				t.Fatalf("branch mode: %q", got)
			}
			if got := test.set.OnCallEnter(control.CallContext{}).Mode; got != control.CheckpointDisabled {
				t.Fatalf("checkpoint mode: %q", got)
			}
			if got := test.set.OnValidationPoint(control.ValidationContext{}).Mode; got != control.ValidationMandatoryFinal {
				t.Fatalf("validation mode: %q", got)
			}
			if got := test.set.OnValidationFail(control.FailureContext{}).Scope; got != control.RecoveryWholeTransaction {
				t.Fatalf("recovery scope: %q", got)
			}
			if got := test.set.OnReplayStart(control.ReplayContext{}).Mode; got != control.ReplayReexecute {
				t.Fatalf("replay mode: %q", got)
			}
		})
	}
}
