package control

import (
	"errors"
	"fmt"
)

type Event string

const (
	EventEpochStart      Event = "EPOCH_START"
	EventBlockReady      Event = "BLOCK_READY"
	EventTxAdmit         Event = "TX_ADMIT"
	EventTxReady         Event = "TX_READY"
	EventTaskReady       Event = "TASK_READY"
	EventCallEnter       Event = "CALL_ENTER"
	EventBranch          Event = "BRANCH"
	EventBeforeRead      Event = "BEFORE_READ"
	EventBeforeWrite     Event = "BEFORE_WRITE"
	EventReadEstimate    Event = "READ_ESTIMATE"
	EventConflict        Event = "CONFLICT"
	EventValidationPoint Event = "VALIDATION_POINT"
	EventTxEnd           Event = "TX_END"
	EventSubtxEnd        Event = "SUBTX_END"
	EventValidationFail  Event = "VALIDATION_FAIL"
	EventReplayStart     Event = "REPLAY_START"
	EventRetryLimit      Event = "RETRY_LIMIT"
	EventWorkerIdle      Event = "WORKER_IDLE"
	EventQueuePressure   Event = "QUEUE_PRESSURE"
)

type TrustClass string

const (
	TrustDeterministic TrustClass = "D"
	TrustLocal         TrustClass = "L"
	TrustHint          TrustClass = "H"
	TrustSafety        TrustClass = "S"
)

type EventDescriptor struct {
	Event        Event      `json:"event"`
	DispatchKey  string     `json:"dispatch_key"`
	Hook         string     `json:"hook"`
	ActionSchema string     `json:"action_schema"`
	TrustClass   TrustClass `json:"trust_class"`
}

var eventRegistry = []EventDescriptor{
	{EventEpochStart, "MacroPolicy.OnEpochStart", "OnEpochStart", "EpochDecision", TrustSafety},
	{EventBlockReady, "MacroPolicy.OnBlockReady", "OnBlockReady", "BlockDecision", TrustDeterministic},
	{EventTxAdmit, "TxPolicy.OnTxAdmit", "OnTxAdmit", "AdmissionDecision", TrustLocal},
	{EventTxReady, "TxPolicy.OnTxReady", "OnTxReady", "AdmissionDecision", TrustLocal},
	{EventTaskReady, "TxPolicy.OnTaskReady", "OnTaskReady", "SchedulingDecision", TrustLocal},
	{EventCallEnter, "RuntimeHooks.OnCallEnter", "OnCallEnter", "CheckpointDecision", TrustHint},
	{EventBranch, "RuntimeHooks.OnBranch", "OnBranch", "BranchDecision", TrustDeterministic},
	{EventBeforeRead, "RuntimeHooks.BeforeRead", "BeforeRead", "AccessDecision", TrustHint},
	{EventBeforeWrite, "RuntimeHooks.BeforeWrite", "BeforeWrite", "AccessDecision", TrustHint},
	{EventReadEstimate, "TxPolicy.OnEstimateRead", "OnEstimateRead", "WaitDecision", TrustLocal},
	{EventConflict, "TxPolicy.OnConflict", "OnConflict", "ConflictDecision", TrustLocal},
	{EventValidationPoint, "RuntimeHooks.OnValidationPoint:POINT", "OnValidationPoint", "ValidationDecision", TrustSafety},
	{EventTxEnd, "RuntimeHooks.OnValidationPoint:TX_END", "OnValidationPoint", "ValidationDecision", TrustSafety},
	{EventSubtxEnd, "RuntimeHooks.OnValidationPoint:SUBTX_END", "OnValidationPoint", "ValidationDecision", TrustSafety},
	{EventValidationFail, "RecoveryPolicy.OnValidationFail", "OnValidationFail", "RecoveryDecision", TrustSafety},
	{EventReplayStart, "RecoveryPolicy.OnReplayStart", "OnReplayStart", "ReplayDecision", TrustSafety},
	{EventRetryLimit, "TxPolicy.OnRetryLimit", "OnRetryLimit", "FallbackDecision", TrustSafety},
	{EventWorkerIdle, "TxPolicy.OnWorkerIdle", "OnWorkerIdle", "ResourceDecision", TrustLocal},
	{EventQueuePressure, "TxPolicy.OnQueuePressure", "OnQueuePressure", "ResourceDecision", TrustLocal},
}

func EventRegistry() []EventDescriptor {
	return append([]EventDescriptor(nil), eventRegistry...)
}

func EventDescription(event Event) (EventDescriptor, bool) {
	for _, descriptor := range eventRegistry {
		if descriptor.Event == event {
			return descriptor, true
		}
	}
	return EventDescriptor{}, false
}

type EventCapability struct {
	Event     Event  `json:"event"`
	Supported bool   `json:"supported"`
	Reason    string `json:"reason,omitempty"`
}

// Capabilities lists every registered event in registry order. Unsupported
// events always carry a reason, so absence of an upstream hook cannot become a
// silent no-op.
type Capabilities struct {
	Engine string            `json:"engine"`
	Events []EventCapability `json:"events"`
}

func NewCapabilities(engine string, supported map[Event]bool, unavailableReasons map[Event]string) (Capabilities, error) {
	if engine == "" {
		return Capabilities{}, errors.New("capability engine name is required")
	}
	capabilities := Capabilities{Engine: engine, Events: make([]EventCapability, 0, len(eventRegistry))}
	for _, descriptor := range eventRegistry {
		available := supported[descriptor.Event]
		reason := unavailableReasons[descriptor.Event]
		if !available && reason == "" {
			return Capabilities{}, fmt.Errorf("event %s is unavailable without a reason", descriptor.Event)
		}
		if available && reason != "" {
			return Capabilities{}, fmt.Errorf("event %s is available but has an unavailable reason", descriptor.Event)
		}
		capabilities.Events = append(capabilities.Events, EventCapability{
			Event:     descriptor.Event,
			Supported: available,
			Reason:    reason,
		})
	}
	for event := range supported {
		if !registeredEvent(event) {
			return Capabilities{}, fmt.Errorf("unknown capability event %q", event)
		}
	}
	for event := range unavailableReasons {
		if !registeredEvent(event) {
			return Capabilities{}, fmt.Errorf("unknown unavailable event %q", event)
		}
	}
	return capabilities, nil
}

func (c Capabilities) Lookup(event Event) (EventCapability, bool) {
	for _, capability := range c.Events {
		if capability.Event == event {
			return capability, true
		}
	}
	return EventCapability{}, false
}

func registeredEvent(event Event) bool {
	for _, descriptor := range eventRegistry {
		if descriptor.Event == event {
			return true
		}
	}
	return false
}
