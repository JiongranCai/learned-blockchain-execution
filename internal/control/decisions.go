package control

type EpochContext struct {
	EpochID string
}

type BlockContext struct {
	BlockID          string
	Height           uint64
	TransactionCount int
}

type TxContext struct {
	BlockID       string
	TransactionID string
	TxIndex       uint64
	Incarnation   uint64
	Ordinal       uint64
}

type TaskContext struct {
	TxContext
	Kind string
}

type ConflictContext struct {
	TxContext
	BlockingTxIndex uint64
	Key             []byte
}

type RetryContext struct {
	TxContext
	RetryCount uint64
}

type ResourceContext struct {
	BlockID string
	Worker  int
	Depth   uint64
	Ordinal uint64
}

type AccessContext struct {
	TxContext
	OperationID    string
	ProgramCounter int
	Key            []byte
}

type BranchContext struct {
	TxContext
	BranchID       string
	ProgramCounter int
	Taken          bool
	Target         int
}

type CallContext struct {
	TxContext
	CallID string
	Path   string
}

type ValidationKind string

const (
	ValidationExplicit ValidationKind = "VALIDATION_POINT"
	ValidationTxEnd    ValidationKind = "TX_END"
	ValidationSubtxEnd ValidationKind = "SUBTX_END"
)

type ValidationContext struct {
	TxContext
	Kind     ValidationKind
	TargetID string
}

type FailureContext struct {
	TxContext
	Reason string
}

type ReplayContext struct {
	TxContext
	Reason string
}

type EpochAction string

const EpochKeepPolicy EpochAction = "keep_policy"

type EpochDecision struct {
	Action EpochAction
}

type BlockAction string

const BlockUseFullWindow BlockAction = "full_block_window"

type BlockDecision struct {
	Action BlockAction
}

type AdmissionLane string

const (
	LaneSerial     AdmissionLane = "serial"
	LaneOptimistic AdmissionLane = "optimistic"
)

type AdmissionDecision struct {
	Lane AdmissionLane
}

type WaitMode string

const (
	WaitNone               WaitMode = "none"
	WaitLogicalPredecessor WaitMode = "logical_predecessor"
)

type WaitDecision struct {
	Mode WaitMode
}

type ConflictMode string

const ConflictValidate ConflictMode = "validate"

type ConflictDecision struct {
	Mode ConflictMode
}

type SchedulingMode string

const SchedulingFIFO SchedulingMode = "fifo"

type SchedulingDecision struct {
	Mode SchedulingMode
}

type FallbackMode string

const FallbackSerial FallbackMode = "serial"

type FallbackDecision struct {
	Mode FallbackMode
}

type ResourceMode string

const ResourceSharedPool ResourceMode = "shared_pool"

type ResourceDecision struct {
	Mode ResourceMode
}

type AccessMode string

const AccessExecute AccessMode = "execute"

type AccessDecision struct {
	Mode AccessMode
}

type BranchMode string

const BranchEvaluateActual BranchMode = "evaluate_actual"

type BranchDecision struct {
	Mode BranchMode
}

type CheckpointMode string

const CheckpointDisabled CheckpointMode = "disabled"

type CheckpointDecision struct {
	Mode CheckpointMode
}

type ValidationMode string

const ValidationMandatoryFinal ValidationMode = "mandatory_final"

type ValidationDecision struct {
	Mode ValidationMode
}

type RecoveryScope string

const RecoveryWholeTransaction RecoveryScope = "whole_transaction"

type RecoveryDecision struct {
	Scope RecoveryScope
}

type ReplayMode string

const ReplayReexecute ReplayMode = "reexecute"

type ReplayDecision struct {
	Mode ReplayMode
}
