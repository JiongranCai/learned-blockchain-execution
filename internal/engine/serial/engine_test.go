package serial_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/crypto-org-chain/go-block-stm/internal/control"
	engineapi "github.com/crypto-org-chain/go-block-stm/internal/engine"
	"github.com/crypto-org-chain/go-block-stm/internal/engine/serial"
	"github.com/crypto-org-chain/go-block-stm/internal/model"
	"github.com/crypto-org-chain/go-block-stm/internal/policy/fixed"
	"github.com/crypto-org-chain/go-block-stm/internal/runtime/flat"
	"github.com/crypto-org-chain/go-block-stm/internal/state/memkv"
)

func TestExecuteBlockSerialVisibilityRollbackAndDeterminism(t *testing.T) {
	initial := []model.StateEntry{{Key: []byte("counter"), Value: flat.EncodeInt64(1)}}
	block := model.Block{
		ID:     "block-000001",
		Height: 1,
		Transactions: []model.Transaction{
			{
				ID:       "tx-1",
				MaxUnits: 3,
				Program: model.Program{Instructions: []model.Instruction{
					{Op: model.OpRead, Key: []byte("counter"), Register: "value"},
					{Op: model.OpWrite, Key: []byte("counter"), Expression: model.Expression{Base: model.Register("value"), Delta: 1}},
					{Op: model.OpReturn, Expression: model.Expression{Base: model.Register("value"), Delta: 1}},
				}},
			},
			{
				ID:       "tx-2",
				MaxUnits: 3,
				Program: model.Program{Instructions: []model.Instruction{
					{Op: model.OpRead, Key: []byte("counter"), Register: "value"},
					{Op: model.OpDelete, Key: []byte("counter")},
					{Op: model.OpFailIf, Condition: model.Condition{Kind: model.ConditionAlways}, ErrorCode: "rollback"},
				}},
			},
			{
				ID:       "tx-3",
				MaxUnits: 2,
				Program: model.Program{Instructions: []model.Instruction{
					{Op: model.OpRead, Key: []byte("counter"), Register: "value"},
					{Op: model.OpReturn, Expression: model.Expression{Base: model.Register("value")}},
				}},
			},
		},
	}

	firstState := mustState(t, initial)
	first, _, err := serial.New(nil).ExecuteBlock(context.Background(), block, firstState, engineapi.RunConfig{Executors: 1})
	if err != nil {
		t.Fatal(err)
	}
	if got := []model.TxStatus{first.Transactions[0].Status, first.Transactions[1].Status, first.Transactions[2].Status}; !reflect.DeepEqual(got, []model.TxStatus{
		model.TxStatusSuccess,
		model.TxStatusFailed,
		model.TxStatusSuccess,
	}) {
		t.Fatalf("unexpected transaction statuses: %v", got)
	}
	if first.Transactions[1].ErrorCode != "rollback" || first.Transactions[1].Writes != nil {
		t.Fatalf("failed transaction did not roll back: %#v", first.Transactions[1])
	}
	if value, ok := flat.DecodeInt64(first.Transactions[2].ReturnValue); !ok || value != 2 {
		t.Fatalf("later transaction observed wrong state: %d, valid=%v", value, ok)
	}
	wantFinal := []model.StateEntry{{Key: []byte("counter"), Value: flat.EncodeInt64(2)}}
	if !reflect.DeepEqual(first.FinalState, wantFinal) || !reflect.DeepEqual(firstState.Snapshot(), wantFinal) {
		t.Fatalf("unexpected final state: result=%#v store=%#v", first.FinalState, firstState.Snapshot())
	}
	if first.Digest == "" || first.Digest != model.CanonicalDigest(first) {
		t.Fatalf("invalid canonical digest: %q", first.Digest)
	}

	secondState := mustState(t, initial)
	second, _, err := serial.New(flat.New()).ExecuteBlock(context.Background(), block, secondState, engineapi.RunConfig{Executors: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("same block/state produced different results:\nfirst: %#v\nsecond: %#v", first, second)
	}
}

func TestExecuteBlockInfrastructureErrorsDoNotPublishState(t *testing.T) {
	initial := []model.StateEntry{{Key: []byte("counter"), Value: flat.EncodeInt64(1)}}
	tests := []struct {
		name    string
		ctx     context.Context
		block   model.Block
		wantErr error
	}{
		{
			name:    "missing block id",
			ctx:     context.Background(),
			block:   model.Block{},
			wantErr: serial.ErrMissingBlockID,
		},
		{
			name: "duplicate transaction id",
			ctx:  context.Background(),
			block: model.Block{ID: "block", Transactions: []model.Transaction{
				{ID: "same"},
				{ID: "same"},
			}},
		},
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	tests = append(tests, struct {
		name    string
		ctx     context.Context
		block   model.Block
		wantErr error
	}{name: "cancelled context", ctx: cancelled, block: model.Block{ID: "block"}, wantErr: context.Canceled})

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			storage := mustState(t, initial)
			_, _, err := serial.New(nil).ExecuteBlock(test.ctx, test.block, storage, engineapi.RunConfig{Executors: 1})
			if test.wantErr != nil {
				if !errors.Is(err, test.wantErr) {
					t.Fatalf("expected %v, got %v", test.wantErr, err)
				}
			} else if err == nil {
				t.Fatal("expected infrastructure error")
			}
			if got := storage.Snapshot(); !reflect.DeepEqual(got, initial) {
				t.Fatalf("infrastructure error published state: %#v", got)
			}
		})
	}
}

func TestExecuteBlockRejectsNilState(t *testing.T) {
	_, _, err := serial.New(nil).ExecuteBlock(context.Background(), model.Block{ID: "block"}, nil, engineapi.RunConfig{Executors: 1})
	if !errors.Is(err, serial.ErrNilState) {
		t.Fatalf("expected ErrNilState, got %v", err)
	}
	var typedNil *memkv.Store
	_, _, err = serial.New(nil).ExecuteBlock(context.Background(), model.Block{ID: "block"}, typedNil, engineapi.RunConfig{Executors: 1})
	if !errors.Is(err, serial.ErrNilState) {
		t.Fatalf("expected ErrNilState for typed nil, got %v", err)
	}
}

func TestSerialTraceUsesSharedPolicySeam(t *testing.T) {
	block := model.Block{
		ID: "trace-block",
		Transactions: []model.Transaction{{
			ID:       "trace-tx",
			MaxUnits: 4,
			Program: model.Program{Instructions: []model.Instruction{
				{ID: "read", Op: model.OpRead, Key: []byte("key"), Register: "value"},
				{ID: "branch", Op: model.OpJumpIf, Condition: model.Condition{Kind: model.ConditionNotExists, Register: "value"}, Target: 3},
				{ID: "wrong", Op: model.OpFailIf, Condition: model.Condition{Kind: model.ConditionAlways}, ErrorCode: "wrong_branch"},
				{ID: "write", Op: model.OpWrite, Key: []byte("key"), Expression: model.Expression{Base: model.Literal(2)}},
				{ID: "return", Op: model.OpReturn, Expression: model.Expression{Base: model.Literal(2)}},
			}},
		}},
	}
	result, trace, err := serial.New(nil).ExecuteBlock(
		context.Background(), block, memkv.New(), engineapi.RunConfig{Executors: 1},
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Transactions[0].Status != model.TxStatusSuccess {
		t.Fatalf("unexpected result: %#v", result.Transactions[0])
	}
	if trace.Engine != "serial" || trace.PolicyName != "SerialPreset" || trace.PolicyVersion == "" {
		t.Fatalf("unexpected trace identity: %#v", trace)
	}
	wantCounts := map[control.Event]int{
		control.EventEpochStart:  1,
		control.EventBlockReady:  1,
		control.EventTxAdmit:     1,
		control.EventTxReady:     1,
		control.EventTaskReady:   1,
		control.EventBranch:      1,
		control.EventBeforeRead:  1,
		control.EventBeforeWrite: 1,
		control.EventTxEnd:       1,
	}
	gotCounts := make(map[control.Event]int)
	for _, event := range trace.Events {
		gotCounts[event.Event]++
	}
	if !reflect.DeepEqual(gotCounts, wantCounts) {
		t.Fatalf("unexpected trace events: got %v want %v", gotCounts, wantCounts)
	}
}

func TestSerialRejectsUnsupportedPresetAndExecutorCountWithoutPublish(t *testing.T) {
	initial := []model.StateEntry{{Key: []byte("stable"), Value: flat.EncodeInt64(1)}}
	block := model.Block{ID: "block", Transactions: []model.Transaction{{
		ID:       "tx",
		MaxUnits: 1,
		Program: model.Program{Instructions: []model.Instruction{{
			Op: model.OpWrite, Key: []byte("stable"), Expression: model.Expression{Base: model.Literal(2)},
		}}},
	}}}
	tests := []struct {
		name   string
		config engineapi.RunConfig
		err    error
	}{
		{name: "optimistic preset", config: engineapi.RunConfig{Executors: 1, Policy: fixed.NewBlockSTMPreset()}, err: engineapi.ErrUnsupported},
		{name: "multiple executors", config: engineapi.RunConfig{Executors: 2}, err: engineapi.ErrInvalidWorkers},
		{name: "negative executors", config: engineapi.RunConfig{Executors: -1}, err: engineapi.ErrInvalidWorkers},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			storage := mustState(t, initial)
			_, _, err := serial.New(nil).ExecuteBlock(context.Background(), block, storage, test.config)
			if !errors.Is(err, test.err) {
				t.Fatalf("expected %v, got %v", test.err, err)
			}
			if got := storage.Snapshot(); !reflect.DeepEqual(got, initial) {
				t.Fatalf("rejected run published state: %#v", got)
			}
		})
	}
}

func TestSerialCapabilitiesCoverRegistry(t *testing.T) {
	capabilities := serial.New(nil).Capabilities()
	if len(capabilities.Events) != len(control.EventRegistry()) {
		t.Fatalf("got %d capabilities, want %d", len(capabilities.Events), len(control.EventRegistry()))
	}
	for _, capability := range capabilities.Events {
		if !capability.Supported && capability.Reason == "" {
			t.Fatalf("unsupported event has no reason: %#v", capability)
		}
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
