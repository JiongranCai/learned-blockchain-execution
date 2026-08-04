package flat_test

import (
	"context"
	"math"
	"reflect"
	"testing"

	"github.com/crypto-org-chain/go-block-stm/internal/model"
	"github.com/crypto-org-chain/go-block-stm/internal/runtime/flat"
	"github.com/crypto-org-chain/go-block-stm/internal/state"
	"github.com/crypto-org-chain/go-block-stm/internal/state/memkv"
)

func TestExecuteSuccessReadsOwnWritesBranchesAndDeletes(t *testing.T) {
	storage := mustState(t, []model.StateEntry{
		{Key: []byte("counter"), Value: flat.EncodeInt64(7)},
		{Key: []byte("doomed"), Value: flat.EncodeInt64(99)},
	})
	view := state.NewOverlay(storage)
	tx := model.Transaction{
		ID:       "tx-success",
		MaxUnits: 9,
		Program: model.Program{Instructions: []model.Instruction{
			{Op: model.OpRead, Key: []byte("counter"), Register: "old"},
			{Op: model.OpWrite, Key: []byte("next"), Expression: model.Expression{Base: model.Register("old"), Delta: 5}},
			{Op: model.OpRead, Key: []byte("next"), Register: "fresh"},
			{Op: model.OpDelete, Key: []byte("doomed")},
			{
				Op: model.OpJumpIf,
				Condition: model.Condition{
					Kind:  model.ConditionEqual,
					Left:  model.Register("fresh"),
					Right: model.Literal(12),
				},
				Target: 6,
			},
			{Op: model.OpFailIf, Condition: model.Condition{Kind: model.ConditionAlways}, ErrorCode: "branch_failed"},
			{Op: model.OpCompute, ComputeUnits: 2},
			{Op: model.OpReturn, Expression: model.Expression{Base: model.Register("fresh")}},
		}},
	}

	result := flat.New().Execute(context.Background(), 4, tx, view)
	if result.Status != model.TxStatusSuccess || result.ErrorCode != "" {
		t.Fatalf("unexpected result: %#v", result)
	}
	if result.Index != 4 || result.TransactionID != tx.ID || result.UnitsUsed != 9 {
		t.Fatalf("unexpected execution accounting: %#v", result)
	}
	if value, ok := flat.DecodeInt64(result.ReturnValue); !ok || value != 12 {
		t.Fatalf("unexpected return value: %d, valid=%v", value, ok)
	}
	wantWrites := []model.StateChange{
		{Key: []byte("doomed"), Delete: true},
		{Key: []byte("next"), Value: flat.EncodeInt64(12)},
	}
	if !reflect.DeepEqual(result.Writes, wantWrites) {
		t.Fatalf("unexpected writes:\n got: %#v\nwant: %#v", result.Writes, wantWrites)
	}
	if len(result.Reads) != 2 || !result.Reads[1].Exists {
		t.Fatalf("read-your-write was not recorded: %#v", result.Reads)
	}
	if _, ok := storage.Get([]byte("next")); ok {
		t.Fatal("runtime published writes without an engine commit")
	}
	if _, ok := storage.Get([]byte("doomed")); !ok {
		t.Fatal("runtime published delete without an engine commit")
	}
}

func TestExecuteFailureSemantics(t *testing.T) {
	tests := []struct {
		name       string
		entries    []model.StateEntry
		maxUnits   uint64
		program    []model.Instruction
		wantStatus model.TxStatus
		wantCode   string
		wantUnits  uint64
	}{
		{
			name:     "explicit failure discards result writes",
			maxUnits: 2,
			program: []model.Instruction{
				{Op: model.OpWrite, Key: []byte("pending"), Expression: model.Expression{Base: model.Literal(1)}},
				{Op: model.OpFailIf, Condition: model.Condition{Kind: model.ConditionAlways}, ErrorCode: "denied"},
			},
			wantStatus: model.TxStatusFailed,
			wantCode:   "denied",
			wantUnits:  2,
		},
		{
			name:     "out of gas discards result writes",
			maxUnits: 3,
			program: []model.Instruction{
				{Op: model.OpWrite, Key: []byte("pending"), Expression: model.Expression{Base: model.Literal(1)}},
				{Op: model.OpCompute, ComputeUnits: 4},
			},
			wantStatus: model.TxStatusOutOfGas,
			wantCode:   flat.ErrorOutOfGas,
			wantUnits:  3,
		},
		{
			name:       "invalid state encoding",
			entries:    []model.StateEntry{{Key: []byte("bad"), Value: []byte{1}}},
			maxUnits:   1,
			program:    []model.Instruction{{Op: model.OpRead, Key: []byte("bad"), Register: "value"}},
			wantStatus: model.TxStatusInvalidState,
			wantCode:   flat.ErrorInvalidStateValue,
			wantUnits:  1,
		},
		{
			name:       "checked arithmetic overflow",
			maxUnits:   1,
			program:    []model.Instruction{{Op: model.OpWrite, Key: []byte("bad"), Expression: model.Expression{Base: model.Literal(math.MaxInt64), Delta: 1}}},
			wantStatus: model.TxStatusArithmeticError,
			wantCode:   flat.ErrorArithmeticOverflow,
			wantUnits:  1,
		},
		{
			name:       "undefined register",
			maxUnits:   1,
			program:    []model.Instruction{{Op: model.OpReturn, Expression: model.Expression{Base: model.Register("missing")}}},
			wantStatus: model.TxStatusInvalidProgram,
			wantCode:   flat.ErrorUndefinedRegister,
			wantUnits:  1,
		},
		{
			name:       "unknown opcode is rejected before charging",
			maxUnits:   1,
			program:    []model.Instruction{{Op: model.Opcode("unknown")}},
			wantStatus: model.TxStatusInvalidProgram,
			wantCode:   flat.ErrorUnknownOpcode,
			wantUnits:  0,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			storage := mustState(t, test.entries)
			view := state.NewOverlay(storage)
			result := flat.New().Execute(context.Background(), 0, model.Transaction{
				ID:       "tx-failure",
				MaxUnits: test.maxUnits,
				Program:  model.Program{Instructions: test.program},
			}, view)
			if result.Status != test.wantStatus || result.ErrorCode != test.wantCode || result.UnitsUsed != test.wantUnits {
				t.Fatalf("unexpected failure result: %#v", result)
			}
			if result.Writes != nil {
				t.Fatalf("failed transaction exposed commit writes: %#v", result.Writes)
			}
			if _, exists := storage.Get([]byte("pending")); exists {
				t.Fatal("failed transaction changed base storage")
			}
		})
	}
}

func TestExecuteMissingReadAndExistsCondition(t *testing.T) {
	storage := memkv.New()
	view := state.NewOverlay(storage)
	tx := model.Transaction{
		ID:       "tx-missing",
		MaxUnits: 3,
		Program: model.Program{Instructions: []model.Instruction{
			{Op: model.OpRead, Key: []byte("missing"), Register: "value"},
			{Op: model.OpFailIf, Condition: model.Condition{Kind: model.ConditionExists, Register: "value"}, ErrorCode: "unexpected_exists"},
			{Op: model.OpReturn, Expression: model.Expression{Base: model.Register("value")}},
		}},
	}

	result := flat.New().Execute(context.Background(), 0, tx, view)
	if result.Status != model.TxStatusSuccess {
		t.Fatalf("missing key should read as zero/nonexistent: %#v", result)
	}
	if len(result.Reads) != 1 || result.Reads[0].Exists || result.Reads[0].Value != nil {
		t.Fatalf("unexpected missing-key record: %#v", result.Reads)
	}
	if value, ok := flat.DecodeInt64(result.ReturnValue); !ok || value != 0 {
		t.Fatalf("unexpected missing-key return: %d, valid=%v", value, ok)
	}
}

func TestAllConditionKinds(t *testing.T) {
	tests := []struct {
		name      string
		condition model.Condition
		program   []model.Instruction
	}{
		{
			name:      "always",
			condition: model.Condition{Kind: model.ConditionAlways},
		},
		{
			name:      "exists",
			program:   []model.Instruction{{Op: model.OpRead, Key: []byte("present"), Register: "read"}},
			condition: model.Condition{Kind: model.ConditionExists, Register: "read"},
		},
		{
			name:      "not exists",
			program:   []model.Instruction{{Op: model.OpRead, Key: []byte("missing"), Register: "read"}},
			condition: model.Condition{Kind: model.ConditionNotExists, Register: "read"},
		},
		{
			name:      "equal",
			condition: model.Condition{Kind: model.ConditionEqual, Left: model.Literal(2), Right: model.Literal(2)},
		},
		{
			name:      "not equal",
			condition: model.Condition{Kind: model.ConditionNotEqual, Left: model.Literal(2), Right: model.Literal(3)},
		},
		{
			name:      "less",
			condition: model.Condition{Kind: model.ConditionLess, Left: model.Literal(2), Right: model.Literal(3)},
		},
		{
			name:      "less equal",
			condition: model.Condition{Kind: model.ConditionLessEqual, Left: model.Literal(2), Right: model.Literal(2)},
		},
		{
			name:      "greater",
			condition: model.Condition{Kind: model.ConditionGreater, Left: model.Literal(3), Right: model.Literal(2)},
		},
		{
			name:      "greater equal",
			condition: model.Condition{Kind: model.ConditionGreaterEq, Left: model.Literal(2), Right: model.Literal(2)},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			instructions := append([]model.Instruction(nil), test.program...)
			instructions = append(instructions, model.Instruction{
				Op:        model.OpFailIf,
				Condition: test.condition,
				ErrorCode: "matched",
			})
			result := flat.New().Execute(context.Background(), 0, model.Transaction{
				ID:       "condition",
				MaxUnits: uint64(len(instructions)),
				Program:  model.Program{Instructions: instructions},
			}, state.NewOverlay(mustState(t, []model.StateEntry{{Key: []byte("present"), Value: flat.EncodeInt64(1)}})))
			if result.Status != model.TxStatusFailed || result.ErrorCode != "matched" {
				t.Fatalf("condition did not match: %#v", result)
			}
		})
	}
}

func TestValidateStructuralErrors(t *testing.T) {
	tests := []struct {
		name     string
		tx       model.Transaction
		wantCode string
	}{
		{
			name:     "zero budget",
			tx:       model.Transaction{},
			wantCode: flat.ErrorInvalidMaxUnits,
		},
		{
			name: "read missing register",
			tx: model.Transaction{MaxUnits: 1, Program: model.Program{Instructions: []model.Instruction{
				{Op: model.OpRead},
			}}},
			wantCode: flat.ErrorMissingRegisterName,
		},
		{
			name: "invalid operand",
			tx: model.Transaction{MaxUnits: 1, Program: model.Program{Instructions: []model.Instruction{
				{Op: model.OpWrite, Expression: model.Expression{Base: model.Operand{Kind: model.OperandKind("invalid")}}},
			}}},
			wantCode: flat.ErrorInvalidOperand,
		},
		{
			name: "register operand missing name",
			tx: model.Transaction{MaxUnits: 1, Program: model.Program{Instructions: []model.Instruction{
				{Op: model.OpReturn, Expression: model.Expression{Base: model.Operand{Kind: model.OperandRegister}}},
			}}},
			wantCode: flat.ErrorMissingRegisterName,
		},
		{
			name: "failure missing code",
			tx: model.Transaction{MaxUnits: 1, Program: model.Program{Instructions: []model.Instruction{
				{Op: model.OpFailIf, Condition: model.Condition{Kind: model.ConditionAlways}},
			}}},
			wantCode: flat.ErrorMissingFailureCode,
		},
		{
			name: "invalid condition",
			tx: model.Transaction{MaxUnits: 1, Program: model.Program{Instructions: []model.Instruction{
				{Op: model.OpFailIf, ErrorCode: "failure", Condition: model.Condition{Kind: model.ConditionKind("invalid")}},
			}}},
			wantCode: flat.ErrorInvalidCondition,
		},
		{
			name: "negative jump",
			tx: model.Transaction{MaxUnits: 1, Program: model.Program{Instructions: []model.Instruction{
				{Op: model.OpJumpIf, Target: -1, Condition: model.Condition{Kind: model.ConditionAlways}},
			}}},
			wantCode: flat.ErrorInvalidJumpTarget,
		},
		{
			name: "jump past end",
			tx: model.Transaction{MaxUnits: 1, Program: model.Program{Instructions: []model.Instruction{
				{Op: model.OpJumpIf, Target: 2, Condition: model.Condition{Kind: model.ConditionAlways}},
			}}},
			wantCode: flat.ErrorInvalidJumpTarget,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := flat.New().Validate(test.tx); got != test.wantCode {
				t.Fatalf("unexpected validation result: got %q want %q", got, test.wantCode)
			}
		})
	}
}

func TestJumpLoopIsBoundedByGas(t *testing.T) {
	result := flat.New().Execute(context.Background(), 0, model.Transaction{
		ID:       "loop",
		MaxUnits: 5,
		Program: model.Program{Instructions: []model.Instruction{
			{Op: model.OpJumpIf, Target: 0, Condition: model.Condition{Kind: model.ConditionAlways}},
		}},
	}, state.NewOverlay(memkv.New()))
	if result.Status != model.TxStatusOutOfGas || result.UnitsUsed != 5 {
		t.Fatalf("loop did not terminate at gas bound: %#v", result)
	}
}

func TestExecuteCancellationAndComputeCostOverflow(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	view := state.NewOverlay(memkv.New())
	result := flat.New().Execute(ctx, 0, model.Transaction{
		ID:       "cancelled",
		MaxUnits: 1,
		Program:  model.Program{},
	}, view)
	if result.Status != model.TxStatusCancelled || result.ErrorCode != flat.ErrorCancelled || result.UnitsUsed != 0 {
		t.Fatalf("unexpected cancellation result: %#v", result)
	}

	result = flat.New().Execute(context.Background(), 0, model.Transaction{
		ID:       "overflow",
		MaxUnits: math.MaxUint64,
		Program: model.Program{Instructions: []model.Instruction{
			{Op: model.OpCompute, ComputeUnits: math.MaxUint64},
		}},
	}, state.NewOverlay(memkv.New()))
	if result.Status != model.TxStatusOutOfGas || result.ErrorCode != flat.ErrorOutOfGas || result.UnitsUsed != math.MaxUint64 {
		t.Fatalf("unexpected compute-cost overflow result: %#v", result)
	}
}

func TestInt64EncodingRoundTrip(t *testing.T) {
	for _, value := range []int64{math.MinInt64, -1, 0, 1, math.MaxInt64} {
		got, ok := flat.DecodeInt64(flat.EncodeInt64(value))
		if !ok || got != value {
			t.Fatalf("round trip failed for %d: got %d, valid=%v", value, got, ok)
		}
	}
	if _, ok := flat.DecodeInt64([]byte{1, 2, 3}); ok {
		t.Fatal("non-eight-byte value was accepted")
	}
}

func mustState(t *testing.T, entries []model.StateEntry) *memkv.Store {
	t.Helper()
	storage, err := memkv.FromEntries(entries)
	if err != nil {
		t.Fatal(err)
	}
	return storage
}
