package control_test

import (
	"reflect"
	"testing"

	"github.com/crypto-org-chain/go-block-stm/internal/control"
)

func TestRegistryCoversWeekThreeEventSetOneToOne(t *testing.T) {
	if err := control.ValidateEventRegistry(); err != nil {
		t.Fatal(err)
	}
	want := []control.Event{
		control.EventEpochStart,
		control.EventBlockReady,
		control.EventTxAdmit,
		control.EventTxReady,
		control.EventTaskReady,
		control.EventCallEnter,
		control.EventBranch,
		control.EventBeforeRead,
		control.EventBeforeWrite,
		control.EventReadEstimate,
		control.EventConflict,
		control.EventValidationPoint,
		control.EventTxEnd,
		control.EventSubtxEnd,
		control.EventValidationFail,
		control.EventReplayStart,
		control.EventRetryLimit,
		control.EventWorkerIdle,
		control.EventQueuePressure,
	}
	registry := control.EventRegistry()
	got := make([]control.Event, len(registry))
	dispatchKeys := make(map[string]struct{}, len(registry))
	for index, descriptor := range registry {
		got[index] = descriptor.Event
		if _, exists := dispatchKeys[descriptor.DispatchKey]; exists {
			t.Fatalf("dispatch key %q is not one-to-one", descriptor.DispatchKey)
		}
		dispatchKeys[descriptor.DispatchKey] = struct{}{}
		if descriptor.Hook == "" || descriptor.ActionSchema == "" || descriptor.TrustClass == "" {
			t.Fatalf("incomplete registry entry: %#v", descriptor)
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("event set mismatch:\n got: %v\nwant: %v", got, want)
	}

	registry[0].Event = "MUTATED_COPY"
	if control.EventRegistry()[0].Event != control.EventEpochStart {
		t.Fatal("EventRegistry exposed mutable registry storage")
	}
}

func TestCapabilitiesRequireExplicitAndConsistentCoverage(t *testing.T) {
	allSupported := make(map[control.Event]bool)
	for _, descriptor := range control.EventRegistry() {
		allSupported[descriptor.Event] = true
	}
	capabilities, err := control.NewCapabilities("test", allSupported, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(capabilities.Events) != len(control.EventRegistry()) {
		t.Fatalf("got %d capabilities", len(capabilities.Events))
	}

	delete(allSupported, control.EventConflict)
	if _, err := control.NewCapabilities("test", allSupported, nil); err == nil {
		t.Fatal("missing unavailable reason was accepted")
	}
	if _, err := control.NewCapabilities("test", allSupported, map[control.Event]string{
		control.EventConflict: "kernel callback unavailable",
	}); err != nil {
		t.Fatalf("explicit unavailable capability was rejected: %v", err)
	}

	allSupported[control.EventConflict] = true
	if _, err := control.NewCapabilities("test", allSupported, map[control.Event]string{
		control.EventConflict: "contradictory reason",
	}); err == nil {
		t.Fatal("supported event with unavailable reason was accepted")
	}
	allSupported[control.Event("UNKNOWN")] = true
	if _, err := control.NewCapabilities("test", allSupported, nil); err == nil {
		t.Fatal("unknown event was accepted")
	}
}
