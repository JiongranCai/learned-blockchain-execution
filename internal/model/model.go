package model

// Opcode identifies one instruction in the deterministic flat runtime.
type Opcode string

const (
	OpRead    Opcode = "read"
	OpWrite   Opcode = "write"
	OpDelete  Opcode = "delete"
	OpCompute Opcode = "compute"
	OpFailIf  Opcode = "fail_if"
	OpJumpIf  Opcode = "jump_if"
	OpReturn  Opcode = "return"
)

// OperandKind identifies how an operand obtains its integer value.
type OperandKind string

const (
	OperandLiteral  OperandKind = "literal"
	OperandRegister OperandKind = "register"
)

type Operand struct {
	Kind     OperandKind `json:"kind"`
	Literal  int64       `json:"literal,omitempty"`
	Register string      `json:"register,omitempty"`
}

func Literal(value int64) Operand {
	return Operand{Kind: OperandLiteral, Literal: value}
}

func Register(name string) Operand {
	return Operand{Kind: OperandRegister, Register: name}
}

// Expression evaluates Base and then adds Delta with checked int64 arithmetic.
type Expression struct {
	Base  Operand `json:"base"`
	Delta int64   `json:"delta,omitempty"`
}

// ConditionKind identifies a deterministic boolean predicate.
type ConditionKind string

const (
	ConditionAlways    ConditionKind = "always"
	ConditionExists    ConditionKind = "exists"
	ConditionNotExists ConditionKind = "not_exists"
	ConditionEqual     ConditionKind = "equal"
	ConditionNotEqual  ConditionKind = "not_equal"
	ConditionLess      ConditionKind = "less"
	ConditionLessEqual ConditionKind = "less_equal"
	ConditionGreater   ConditionKind = "greater"
	ConditionGreaterEq ConditionKind = "greater_equal"
)

type Condition struct {
	Kind     ConditionKind `json:"kind"`
	Register string        `json:"register,omitempty"`
	Left     Operand       `json:"left,omitempty"`
	Right    Operand       `json:"right,omitempty"`
}

type Instruction struct {
	Op           Opcode     `json:"op"`
	Key          []byte     `json:"key,omitempty"`
	Register     string     `json:"register,omitempty"`
	Expression   Expression `json:"expression,omitempty"`
	Condition    Condition  `json:"condition,omitempty"`
	Target       int        `json:"target,omitempty"`
	ComputeUnits uint64     `json:"compute_units,omitempty"`
	ErrorCode    string     `json:"error_code,omitempty"`
}

type Program struct {
	Instructions []Instruction `json:"instructions"`
}

type Transaction struct {
	ID       string  `json:"id"`
	MaxUnits uint64  `json:"max_units"`
	Program  Program `json:"program"`
}

type Block struct {
	ID           string        `json:"id"`
	Height       uint64        `json:"height"`
	Transactions []Transaction `json:"transactions"`
}

type TxStatus string

const (
	TxStatusSuccess         TxStatus = "success"
	TxStatusFailed          TxStatus = "failed"
	TxStatusOutOfGas        TxStatus = "out_of_gas"
	TxStatusInvalidProgram  TxStatus = "invalid_program"
	TxStatusInvalidState    TxStatus = "invalid_state"
	TxStatusArithmeticError TxStatus = "arithmetic_error"
	TxStatusCancelled       TxStatus = "cancelled"
)

type ReadRecord struct {
	Key    []byte `json:"key"`
	Exists bool   `json:"exists"`
	Value  []byte `json:"value,omitempty"`
}

type StateChange struct {
	Key    []byte `json:"key"`
	Delete bool   `json:"delete,omitempty"`
	Value  []byte `json:"value,omitempty"`
}

type Event struct {
	Type string `json:"type"`
	Data []byte `json:"data,omitempty"`
}

type TxResult struct {
	Index         uint64        `json:"index"`
	TransactionID string        `json:"transaction_id"`
	Status        TxStatus      `json:"status"`
	ReturnValue   []byte        `json:"return_value,omitempty"`
	ErrorCode     string        `json:"error_code,omitempty"`
	UnitsUsed     uint64        `json:"units_used"`
	ComputeDigest uint64        `json:"compute_digest"`
	Reads         []ReadRecord  `json:"reads,omitempty"`
	Writes        []StateChange `json:"writes,omitempty"`
	Events        []Event       `json:"events,omitempty"`
}

type StateEntry struct {
	Key   []byte `json:"key"`
	Value []byte `json:"value"`
}

type BlockResult struct {
	BlockID      string       `json:"block_id"`
	Height       uint64       `json:"height"`
	Transactions []TxResult   `json:"transactions"`
	FinalState   []StateEntry `json:"final_state"`
	Digest       string       `json:"digest"`
}
