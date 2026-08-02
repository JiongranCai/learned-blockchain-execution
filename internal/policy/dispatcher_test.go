package policy_test

import (
	"errors"
	"testing"

	"github.com/crypto-org-chain/go-block-stm/internal/control"
	"github.com/crypto-org-chain/go-block-stm/internal/policy"
	"github.com/crypto-org-chain/go-block-stm/internal/policy/fixed"
)

func TestDispatcherMapsEveryEventToItsTypedHook(t *testing.T) {
	dispatcher, err := policy.NewDispatcher(fixed.NewBlockSTMPreset())
	if err != nil {
		t.Fatal(err)
	}
	tx := control.TxContext{
		BlockID:       "block",
		TransactionID: "tx",
		TxIndex:       3,
		Incarnation:   1,
	}

	dispatcher.OnEpochStart(control.EpochContext{EpochID: "epoch"})
	dispatcher.OnBlockReady(control.BlockContext{BlockID: "block"})
	dispatcher.OnTxAdmit(tx)
	dispatcher.OnTxReady(tx)
	dispatcher.OnTaskReady(control.TaskContext{TxContext: tx, Kind: "execution"})
	dispatcher.OnCallEnter(control.CallContext{TxContext: tx, CallID: "call"})
	dispatcher.OnBranch(control.BranchContext{TxContext: tx, BranchID: "branch"})
	dispatcher.BeforeRead(control.AccessContext{TxContext: tx, OperationID: "read"})
	dispatcher.BeforeWrite(control.AccessContext{TxContext: tx, OperationID: "write"})
	dispatcher.OnEstimateRead(control.ConflictContext{TxContext: tx, BlockingTxIndex: 2})
	dispatcher.OnConflict(control.ConflictContext{TxContext: tx, BlockingTxIndex: 2})
	dispatcher.OnValidationPoint(control.ValidationContext{TxContext: tx, Kind: control.ValidationExplicit, TargetID: "point"})
	dispatcher.OnValidationPoint(control.ValidationContext{TxContext: tx, Kind: control.ValidationTxEnd, TargetID: "tx"})
	dispatcher.OnValidationPoint(control.ValidationContext{TxContext: tx, Kind: control.ValidationSubtxEnd, TargetID: "subtx"})
	dispatcher.OnValidationFail(control.FailureContext{TxContext: tx, Reason: "changed"})
	dispatcher.OnReplayStart(control.ReplayContext{TxContext: tx, Reason: "retry"})
	dispatcher.OnRetryLimit(control.RetryContext{TxContext: tx, RetryCount: 9})
	dispatcher.OnWorkerIdle(control.ResourceContext{BlockID: "block", Worker: 1})
	dispatcher.OnQueuePressure(control.ResourceContext{BlockID: "block", Depth: 5})

	if err := dispatcher.Err(); err != nil {
		t.Fatal(err)
	}
	trace := dispatcher.Trace("test-engine")
	if trace.PolicyName != "BlockSTMPreset" || trace.PolicyVersion == "" || trace.Engine != "test-engine" {
		t.Fatalf("unexpected trace identity: %#v", trace)
	}
	counts := make(map[control.Event]int)
	for _, record := range trace.Events {
		if record.TrustClass == "" || record.FeatureSource != "fixed_config" || record.ObservationVersion != "none" || record.PolicyTableVersion == "" {
			t.Fatalf("event provenance is incomplete: %#v", record)
		}
		counts[record.Event]++
	}
	for _, descriptor := range control.EventRegistry() {
		if counts[descriptor.Event] != 1 {
			t.Fatalf("event %s dispatched %d times, want once", descriptor.Event, counts[descriptor.Event])
		}
	}
	if len(trace.ActionCounters) != len(control.EventRegistry()) {
		t.Fatalf("got %d action counters, want %d", len(trace.ActionCounters), len(control.EventRegistry()))
	}
}

func TestDispatcherRejectsInvalidIdentityDecisionAndValidationKind(t *testing.T) {
	if _, err := policy.NewDispatcher(nil); !errors.Is(err, policy.ErrInvalidPolicy) {
		t.Fatalf("nil policy: got %v", err)
	}
	var typedNil *fixed.Preset
	if _, err := policy.NewDispatcher(typedNil); !errors.Is(err, policy.ErrInvalidPolicy) {
		t.Fatalf("typed-nil policy: got %v", err)
	}
	if _, err := policy.NewDispatcher(emptyIdentityPolicy{Policy: fixed.NewSerialPreset()}); !errors.Is(err, policy.ErrInvalidPolicy) {
		t.Fatalf("empty identity: got %v", err)
	}

	dispatcher, err := policy.NewDispatcher(invalidAdmissionPolicy{Policy: fixed.NewSerialPreset()})
	if err != nil {
		t.Fatal(err)
	}
	dispatcher.OnTxAdmit(control.TxContext{TransactionID: "tx"})
	if !errors.Is(dispatcher.Err(), policy.ErrInvalidDecision) {
		t.Fatalf("invalid admission decision: got %v", dispatcher.Err())
	}

	dispatcher, err = policy.NewDispatcher(fixed.NewSerialPreset())
	if err != nil {
		t.Fatal(err)
	}
	dispatcher.OnValidationPoint(control.ValidationContext{Kind: control.ValidationKind("UNKNOWN")})
	if !errors.Is(dispatcher.Err(), policy.ErrInvalidDecision) {
		t.Fatalf("invalid validation kind: got %v", dispatcher.Err())
	}
}

type invalidAdmissionPolicy struct {
	policy.Policy
}

func (invalidAdmissionPolicy) OnTxAdmit(control.TxContext) control.AdmissionDecision {
	return control.AdmissionDecision{}
}

type emptyIdentityPolicy struct {
	policy.Policy
}

func (emptyIdentityPolicy) Identity() policy.Identity {
	return policy.Identity{}
}
