package flat

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"math/bits"

	"github.com/crypto-org-chain/go-block-stm/internal/control"
	"github.com/crypto-org-chain/go-block-stm/internal/model"
	"github.com/crypto-org-chain/go-block-stm/internal/policy"
	"github.com/crypto-org-chain/go-block-stm/internal/state"
)

const (
	ErrorInvalidMaxUnits     = "invalid_max_units"
	ErrorUnknownOpcode       = "unknown_opcode"
	ErrorMissingRegisterName = "missing_register_name"
	ErrorUndefinedRegister   = "undefined_register"
	ErrorInvalidOperand      = "invalid_operand"
	ErrorInvalidCondition    = "invalid_condition"
	ErrorInvalidJumpTarget   = "invalid_jump_target"
	ErrorMissingFailureCode  = "missing_failure_code"
	ErrorInvalidStateValue   = "invalid_state_value"
	ErrorArithmeticOverflow  = "arithmetic_overflow"
	ErrorOutOfGas            = "out_of_gas"
	ErrorCancelled           = "cancelled"
)

const initialComputeDigest uint64 = 0xcbf29ce484222325

type Runtime struct{}

type registerValue struct {
	value  int64
	exists bool
}

func New() *Runtime {
	return &Runtime{}
}

// Validate checks structural program errors that do not depend on runtime state.
// The returned string is a stable error code; an empty string means valid.
func (r *Runtime) Validate(tx model.Transaction) string {
	if tx.MaxUnits == 0 {
		return ErrorInvalidMaxUnits
	}

	programLength := len(tx.Program.Instructions)
	for _, instruction := range tx.Program.Instructions {
		switch instruction.Op {
		case model.OpRead:
			if instruction.Register == "" {
				return ErrorMissingRegisterName
			}
		case model.OpWrite, model.OpReturn:
			if code := validateOperand(instruction.Expression.Base); code != "" {
				return code
			}
		case model.OpDelete, model.OpCompute:
		case model.OpFailIf:
			if instruction.ErrorCode == "" {
				return ErrorMissingFailureCode
			}
			if code := validateCondition(instruction.Condition); code != "" {
				return code
			}
		case model.OpJumpIf:
			if instruction.Target < 0 || instruction.Target > programLength {
				return ErrorInvalidJumpTarget
			}
			if code := validateCondition(instruction.Condition); code != "" {
				return code
			}
		default:
			return ErrorUnknownOpcode
		}
	}
	return ""
}

// Execute runs one transaction against an isolated overlay. The caller commits
// the overlay only when Status is TxStatusSuccess.
func (r *Runtime) Execute(
	ctx context.Context,
	index uint64,
	tx model.Transaction,
	view *state.Overlay,
) model.TxResult {
	return r.ExecuteWithHooks(ctx, control.TxContext{
		TransactionID: tx.ID,
		TxIndex:       index,
	}, tx, view, policy.RuntimeDefaults{})
}

// ExecuteWithHooks runs the same deterministic semantics through the shared
// typed runtime-policy seam. Hooks may observe/select the current baseline
// access/branch actions but cannot directly mutate state.
func (r *Runtime) ExecuteWithHooks(
	ctx context.Context,
	execution control.TxContext,
	tx model.Transaction,
	view *state.Overlay,
	hooks policy.RuntimeHooks,
) model.TxResult {
	if hooks == nil {
		hooks = policy.RuntimeDefaults{}
	}
	if execution.TransactionID == "" {
		execution.TransactionID = tx.ID
	}
	result := model.TxResult{
		Index:         execution.TxIndex,
		TransactionID: tx.ID,
		ComputeDigest: initialComputeDigest,
	}

	if cancelled(ctx) {
		result.Status = model.TxStatusCancelled
		result.ErrorCode = ErrorCancelled
		return result
	}
	if code := r.Validate(tx); code != "" {
		result.Status = model.TxStatusInvalidProgram
		result.ErrorCode = code
		return result
	}

	registers := make(map[string]registerValue)
	pc := 0
	ordinal := execution.Ordinal
	for pc < len(tx.Program.Instructions) {
		if cancelled(ctx) {
			result.Status = model.TxStatusCancelled
			result.ErrorCode = ErrorCancelled
			return result
		}

		instruction := tx.Program.Instructions[pc]
		cost, overflow := instructionCost(instruction)
		if overflow || cost > tx.MaxUnits-result.UnitsUsed {
			result.UnitsUsed = tx.MaxUnits
			result.Status = model.TxStatusOutOfGas
			result.ErrorCode = ErrorOutOfGas
			return result
		}
		result.UnitsUsed += cost

		switch instruction.Op {
		case model.OpRead:
			ordinal++
			hookContext := execution
			hookContext.Ordinal = ordinal
			hooks.BeforeRead(control.AccessContext{
				TxContext:      hookContext,
				OperationID:    operationID(tx.ID, instruction.ID, pc),
				ProgramCounter: pc,
				Key:            cloneBytes(instruction.Key),
			})
			raw, exists := view.Get(instruction.Key)
			value := int64(0)
			if exists {
				var ok bool
				value, ok = DecodeInt64(raw)
				if !ok {
					result.Status = model.TxStatusInvalidState
					result.ErrorCode = ErrorInvalidStateValue
					return result
				}
			}
			registers[instruction.Register] = registerValue{value: value, exists: exists}
			result.Reads = append(result.Reads, model.ReadRecord{
				Key:    cloneBytes(instruction.Key),
				Exists: exists,
				Value:  cloneBytes(raw),
			})
			pc++

		case model.OpWrite:
			ordinal++
			hookContext := execution
			hookContext.Ordinal = ordinal
			hooks.BeforeWrite(control.AccessContext{
				TxContext:      hookContext,
				OperationID:    operationID(tx.ID, instruction.ID, pc),
				ProgramCounter: pc,
				Key:            cloneBytes(instruction.Key),
			})
			value, code := evaluateExpression(instruction.Expression, registers)
			if code != "" {
				setEvaluationError(&result, code)
				return result
			}
			view.Set(instruction.Key, EncodeInt64(value))
			pc++

		case model.OpDelete:
			ordinal++
			hookContext := execution
			hookContext.Ordinal = ordinal
			hooks.BeforeWrite(control.AccessContext{
				TxContext:      hookContext,
				OperationID:    operationID(tx.ID, instruction.ID, pc),
				ProgramCounter: pc,
				Key:            cloneBytes(instruction.Key),
			})
			view.Delete(instruction.Key)
			pc++

		case model.OpCompute:
			for i := uint64(0); i < instruction.ComputeUnits; i++ {
				if i&1023 == 0 && cancelled(ctx) {
					result.Status = model.TxStatusCancelled
					result.ErrorCode = ErrorCancelled
					return result
				}
				result.ComputeDigest ^= i + uint64(pc) + 0x9e3779b97f4a7c15
				result.ComputeDigest *= 0x100000001b3
				result.ComputeDigest = bits.RotateLeft64(result.ComputeDigest, 13)
			}
			pc++

		case model.OpFailIf:
			matched, code := evaluateCondition(instruction.Condition, registers)
			if code != "" {
				setEvaluationError(&result, code)
				return result
			}
			if matched {
				result.Status = model.TxStatusFailed
				result.ErrorCode = instruction.ErrorCode
				return result
			}
			pc++

		case model.OpJumpIf:
			matched, code := evaluateCondition(instruction.Condition, registers)
			if code != "" {
				setEvaluationError(&result, code)
				return result
			}
			ordinal++
			hookContext := execution
			hookContext.Ordinal = ordinal
			hooks.OnBranch(control.BranchContext{
				TxContext:      hookContext,
				BranchID:       operationID(tx.ID, instruction.ID, pc),
				ProgramCounter: pc,
				Taken:          matched,
				Target:         instruction.Target,
			})
			if matched {
				pc = instruction.Target
			} else {
				pc++
			}

		case model.OpReturn:
			value, code := evaluateExpression(instruction.Expression, registers)
			if code != "" {
				setEvaluationError(&result, code)
				return result
			}
			result.Status = model.TxStatusSuccess
			result.ReturnValue = EncodeInt64(value)
			result.Writes = view.Changes()
			return result
		}
	}

	result.Status = model.TxStatusSuccess
	result.Writes = view.Changes()
	return result
}

func operationID(transactionID, instructionID string, programCounter int) string {
	if instructionID != "" {
		return instructionID
	}
	return fmt.Sprintf("%s/op-%d", transactionID, programCounter)
}

func instructionCost(instruction model.Instruction) (uint64, bool) {
	if instruction.Op != model.OpCompute {
		return 1, false
	}
	if instruction.ComputeUnits == math.MaxUint64 {
		return 0, true
	}
	return instruction.ComputeUnits + 1, false
}

func validateOperand(operand model.Operand) string {
	switch operand.Kind {
	case model.OperandLiteral:
		return ""
	case model.OperandRegister:
		if operand.Register == "" {
			return ErrorMissingRegisterName
		}
		return ""
	default:
		return ErrorInvalidOperand
	}
}

func validateCondition(condition model.Condition) string {
	switch condition.Kind {
	case model.ConditionAlways:
		return ""
	case model.ConditionExists, model.ConditionNotExists:
		if condition.Register == "" {
			return ErrorMissingRegisterName
		}
		return ""
	case model.ConditionEqual,
		model.ConditionNotEqual,
		model.ConditionLess,
		model.ConditionLessEqual,
		model.ConditionGreater,
		model.ConditionGreaterEq:
		if code := validateOperand(condition.Left); code != "" {
			return code
		}
		return validateOperand(condition.Right)
	default:
		return ErrorInvalidCondition
	}
}

func evaluateExpression(
	expression model.Expression,
	registers map[string]registerValue,
) (int64, string) {
	base, code := evaluateOperand(expression.Base, registers)
	if code != "" {
		return 0, code
	}
	if expression.Delta > 0 && base > math.MaxInt64-expression.Delta {
		return 0, ErrorArithmeticOverflow
	}
	if expression.Delta < 0 && base < math.MinInt64-expression.Delta {
		return 0, ErrorArithmeticOverflow
	}
	return base + expression.Delta, ""
}

func evaluateOperand(operand model.Operand, registers map[string]registerValue) (int64, string) {
	switch operand.Kind {
	case model.OperandLiteral:
		return operand.Literal, ""
	case model.OperandRegister:
		value, ok := registers[operand.Register]
		if !ok {
			return 0, ErrorUndefinedRegister
		}
		return value.value, ""
	default:
		return 0, ErrorInvalidOperand
	}
}

func evaluateCondition(
	condition model.Condition,
	registers map[string]registerValue,
) (bool, string) {
	switch condition.Kind {
	case model.ConditionAlways:
		return true, ""
	case model.ConditionExists, model.ConditionNotExists:
		value, ok := registers[condition.Register]
		if !ok {
			return false, ErrorUndefinedRegister
		}
		if condition.Kind == model.ConditionExists {
			return value.exists, ""
		}
		return !value.exists, ""
	}

	left, code := evaluateOperand(condition.Left, registers)
	if code != "" {
		return false, code
	}
	right, code := evaluateOperand(condition.Right, registers)
	if code != "" {
		return false, code
	}

	switch condition.Kind {
	case model.ConditionEqual:
		return left == right, ""
	case model.ConditionNotEqual:
		return left != right, ""
	case model.ConditionLess:
		return left < right, ""
	case model.ConditionLessEqual:
		return left <= right, ""
	case model.ConditionGreater:
		return left > right, ""
	case model.ConditionGreaterEq:
		return left >= right, ""
	default:
		return false, ErrorInvalidCondition
	}
}

func setEvaluationError(result *model.TxResult, code string) {
	result.ErrorCode = code
	if code == ErrorArithmeticOverflow {
		result.Status = model.TxStatusArithmeticError
		return
	}
	result.Status = model.TxStatusInvalidProgram
}

func EncodeInt64(value int64) []byte {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], uint64(value))
	return encoded[:]
}

func DecodeInt64(encoded []byte) (int64, bool) {
	if len(encoded) != 8 {
		return 0, false
	}
	return int64(binary.BigEndian.Uint64(encoded)), true
}

func cancelled(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return true
	default:
		return false
	}
}

func cloneBytes(value []byte) []byte {
	if value == nil {
		return nil
	}
	return append([]byte(nil), value...)
}
