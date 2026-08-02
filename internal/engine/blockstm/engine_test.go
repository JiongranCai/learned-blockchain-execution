package blockstm_test

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"sync"
	"testing"

	"github.com/crypto-org-chain/go-block-stm/internal/control"
	engineapi "github.com/crypto-org-chain/go-block-stm/internal/engine"
	"github.com/crypto-org-chain/go-block-stm/internal/engine/blockstm"
	"github.com/crypto-org-chain/go-block-stm/internal/engine/serial"
	"github.com/crypto-org-chain/go-block-stm/internal/model"
	"github.com/crypto-org-chain/go-block-stm/internal/policy"
	"github.com/crypto-org-chain/go-block-stm/internal/policy/fixed"
	"github.com/crypto-org-chain/go-block-stm/internal/runtime/flat"
	"github.com/crypto-org-chain/go-block-stm/internal/state/memkv"
	"github.com/crypto-org-chain/go-block-stm/internal/workload/synthetic"
)

func TestAdversarialBlockMatchesSerialPreset(t *testing.T) {
	initial := []model.StateEntry{
		{Key: []byte("doomed"), Value: flat.EncodeInt64(5)},
		{Key: []byte("hot"), Value: flat.EncodeInt64(0)},
		{Key: []byte("invalid"), Value: []byte{}},
		{Key: []byte("max"), Value: flat.EncodeInt64(math.MaxInt64)},
	}
	block := adversarialBlock()

	serialResult, blockSTMResult, serialState, blockSTMState := executeBoth(t, block, initial, 4)
	assertEquivalent(t, serialResult, blockSTMResult, serialState, blockSTMState)

	wantStatuses := []model.TxStatus{
		model.TxStatusSuccess,
		model.TxStatusSuccess,
		model.TxStatusFailed,
		model.TxStatusSuccess,
		model.TxStatusInvalidState,
		model.TxStatusOutOfGas,
		model.TxStatusArithmeticError,
		model.TxStatusSuccess,
	}
	gotStatuses := make([]model.TxStatus, len(blockSTMResult.Transactions))
	for index, result := range blockSTMResult.Transactions {
		gotStatuses[index] = result.Status
	}
	if !reflect.DeepEqual(gotStatuses, wantStatuses) {
		t.Fatalf("unexpected adversarial statuses: got %v want %v", gotStatuses, wantStatuses)
	}
	if _, exists := blockSTMState.Get([]byte("pending-failure")); exists {
		t.Fatal("explicit failure leaked a write")
	}
	if _, exists := blockSTMState.Get([]byte("pending-oog")); exists {
		t.Fatal("out-of-gas transaction leaked a write")
	}
}

func TestGeneratedArtifactsMatchAcrossSeedsAndWorkers(t *testing.T) {
	// Zero deliberately exercises the frozen kernel's original default-worker
	// path (min(GOMAXPROCS, NumCPU)).
	workers := []int{0, 1, 2, 4, 8}
	for seed := int64(0); seed < 8; seed++ {
		artifact, err := synthetic.Generate(synthetic.Config{
			Seed:                 seed,
			InitialKeys:          8,
			KeySpace:             4,
			BlockCount:           2,
			TransactionsPerBlock: 24,
			MaxComputeUnits:      32,
			TransactionMaxUnits:  36,
			FailureEvery:         7,
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, executorCount := range workers {
			t.Run(testName(seed, executorCount), func(t *testing.T) {
				serialState, err := artifact.NewState()
				if err != nil {
					t.Fatal(err)
				}
				blockSTMState, err := artifact.NewState()
				if err != nil {
					t.Fatal(err)
				}
				serialEngine := serial.New(nil)
				blockSTMEngine := blockstm.New(nil)
				for _, block := range artifact.OrderedBlocks {
					serialResult, _, err := serialEngine.ExecuteBlock(
						context.Background(), block, serialState,
						engineapi.RunConfig{Executors: 1},
					)
					if err != nil {
						t.Fatal(err)
					}
					blockSTMResult, _, err := blockSTMEngine.ExecuteBlock(
						context.Background(), block, blockSTMState,
						engineapi.RunConfig{Executors: executorCount},
					)
					if err != nil {
						t.Fatal(err)
					}
					assertEquivalent(t, serialResult, blockSTMResult, serialState, blockSTMState)
				}
			})
		}
	}
}

func TestFiniteSpeculationLimitsMatchSerialAcrossSeedsAndWorkers(t *testing.T) {
	for seed := int64(0); seed < 4; seed++ {
		artifact, err := synthetic.Generate(synthetic.Config{
			Seed:                 seed + 100,
			InitialKeys:          16,
			KeySpace:             int(seed%4) + 1,
			BlockCount:           2,
			TransactionsPerBlock: 32,
			MaxComputeUnits:      64,
			TransactionMaxUnits:  68,
			FailureEvery:         9,
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, workers := range []int{2, 4, 8} {
			for _, limit := range []int{1, 2, 3, workers, workers * 4, 0} {
				t.Run(fmt.Sprintf("seed-%d-workers-%d-limit-%d", seed, workers, limit), func(t *testing.T) {
					serialState, err := artifact.NewState()
					if err != nil {
						t.Fatal(err)
					}
					candidateState, err := artifact.NewState()
					if err != nil {
						t.Fatal(err)
					}
					for _, block := range artifact.OrderedBlocks {
						oracle, _, err := serial.New(nil).ExecuteBlock(
							context.Background(), block, serialState, engineapi.RunConfig{Executors: 1},
						)
						if err != nil {
							t.Fatal(err)
						}
						candidate, _, err := blockstm.New(nil).ExecuteBlock(
							context.Background(), block, candidateState,
							engineapi.RunConfig{Executors: workers, MaxSpeculativeInflight: limit},
						)
						if err != nil {
							t.Fatal(err)
						}
						assertEquivalent(t, oracle, candidate, serialState, candidateState)
					}
				})
			}
		}
	}
}

func TestFiniteSpeculationLimitEmitsBoundedAdmissionTelemetry(t *testing.T) {
	artifact, err := synthetic.Generate(synthetic.Config{
		Seed:                 808,
		InitialKeys:          4,
		KeySpace:             1,
		BlockCount:           1,
		TransactionsPerBlock: 64,
		MaxComputeUnits:      128,
		TransactionMaxUnits:  132,
		FailureEvery:         0,
	})
	if err != nil {
		t.Fatal(err)
	}
	serialState, err := artifact.NewState()
	if err != nil {
		t.Fatal(err)
	}
	oracle, _, err := serial.New(nil).ExecuteBlock(
		context.Background(), artifact.OrderedBlocks[0], serialState,
		engineapi.RunConfig{Executors: 1, TraceMode: control.TraceCounters},
	)
	if err != nil {
		t.Fatal(err)
	}
	candidateState, err := artifact.NewState()
	if err != nil {
		t.Fatal(err)
	}
	candidate, trace, err := blockstm.New(nil).ExecuteBlock(
		context.Background(), artifact.OrderedBlocks[0], candidateState,
		engineapi.RunConfig{Executors: 8, MaxSpeculativeInflight: 4, TraceMode: control.TraceCounters},
	)
	if err != nil {
		t.Fatal(err)
	}
	assertEquivalent(t, oracle, candidate, serialState, candidateState)
	if !trace.WorkAvailable || !trace.Work.SpeculationLimitApplied || !trace.Work.SpeculationTelemetryAvailable ||
		trace.Work.SpeculationLimit != 4 || trace.Work.PeakSpeculativeInflight == 0 || trace.Work.PeakSpeculativeInflight > 4 ||
		trace.Work.AdmissionStallEvents == 0 || trace.Work.AdmissionStallNS == 0 {
		t.Fatalf("invalid CQ2 telemetry: %#v", trace.Work)
	}
}

func TestEngineRejectsNegativeSpeculationLimit(t *testing.T) {
	storage := mustState(t, []model.StateEntry{{Key: []byte("hot"), Value: flat.EncodeInt64(0)}})
	_, _, err := blockstm.New(nil).ExecuteBlock(
		context.Background(), adversarialBlock(), storage,
		engineapi.RunConfig{Executors: 2, MaxSpeculativeInflight: -1},
	)
	if !errors.Is(err, engineapi.ErrInvalidSpeculationLimit) {
		t.Fatalf("got %v, want invalid speculation limit", err)
	}
}

func TestCancellationAndUnsupportedPolicyDoNotPublishState(t *testing.T) {
	initial := []model.StateEntry{{Key: []byte("hot"), Value: flat.EncodeInt64(0)}}
	block := adversarialBlock()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	storage := mustState(t, initial)
	_, _, err := blockstm.New(nil).ExecuteBlock(ctx, block, storage, engineapi.RunConfig{Executors: 4})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation, got %v", err)
	}
	if !reflect.DeepEqual(storage.Snapshot(), initial) {
		t.Fatalf("cancelled block published state: %#v", storage.Snapshot())
	}

	storage = mustState(t, initial)
	_, _, err = blockstm.New(nil).ExecuteBlock(context.Background(), block, storage, engineapi.RunConfig{
		Executors: 4,
		Policy:    fixed.NewSerialPreset(),
	})
	if !errors.Is(err, engineapi.ErrUnsupported) {
		t.Fatalf("expected unsupported policy error, got %v", err)
	}
	if !reflect.DeepEqual(storage.Snapshot(), initial) {
		t.Fatalf("unsupported policy published state: %#v", storage.Snapshot())
	}
}

func TestEngineCanExecuteIndependentBlocksConcurrently(t *testing.T) {
	artifact, err := synthetic.Generate(synthetic.Config{
		Seed:                 77,
		InitialKeys:          6,
		KeySpace:             3,
		BlockCount:           1,
		TransactionsPerBlock: 32,
		MaxComputeUnits:      16,
		TransactionMaxUnits:  20,
		FailureEvery:         5,
	})
	if err != nil {
		t.Fatal(err)
	}
	engine := blockstm.New(nil)
	const runs = 12
	results := make([]model.BlockResult, runs)
	states := make([][]model.StateEntry, runs)
	errorsByRun := make([]error, runs)
	var wait sync.WaitGroup
	wait.Add(runs)
	for run := 0; run < runs; run++ {
		go func(index int) {
			defer wait.Done()
			storage, stateErr := artifact.NewState()
			if stateErr != nil {
				errorsByRun[index] = stateErr
				return
			}
			result, _, executeErr := engine.ExecuteBlock(
				context.Background(), artifact.OrderedBlocks[0], storage,
				engineapi.RunConfig{Executors: 4},
			)
			results[index] = result
			states[index] = storage.Snapshot()
			errorsByRun[index] = executeErr
		}(run)
	}
	wait.Wait()
	for run := 0; run < runs; run++ {
		if errorsByRun[run] != nil {
			t.Fatalf("run %d: %v", run, errorsByRun[run])
		}
		if !reflect.DeepEqual(results[0], results[run]) || !reflect.DeepEqual(states[0], states[run]) {
			t.Fatalf("concurrent run %d diverged", run)
		}
	}
}

func TestCapabilitiesCoverRegistryAndExposeKernelBoundary(t *testing.T) {
	capabilities := blockstm.New(nil).Capabilities()
	if len(capabilities.Events) != len(control.EventRegistry()) {
		t.Fatalf("capability/event mismatch: got %d want %d", len(capabilities.Events), len(control.EventRegistry()))
	}
	for _, event := range []control.Event{control.EventReadEstimate, control.EventConflict, control.EventWorkerIdle, control.EventQueuePressure} {
		capability, ok := capabilities.Lookup(event)
		if !ok || capability.Supported || capability.Reason == "" {
			t.Fatalf("hidden kernel event was not explicitly unavailable: %#v", capability)
		}
	}
	for _, event := range []control.Event{control.EventBeforeRead, control.EventBeforeWrite, control.EventTxEnd, control.EventValidationFail, control.EventReplayStart} {
		capability, ok := capabilities.Lookup(event)
		if !ok || !capability.Supported {
			t.Fatalf("adapter event should be supported: %#v", capability)
		}
	}
}

func TestConflictReexecutionIsExposedThroughRecoveryHooks(t *testing.T) {
	initial := []model.StateEntry{{Key: []byte("hot"), Value: flat.EncodeInt64(0)}}
	block := model.Block{
		ID: "reexecution-block",
		Transactions: []model.Transaction{
			transaction("blocked-predecessor", 3,
				model.Instruction{Op: model.OpRead, Key: []byte("hot"), Register: "value"},
				model.Instruction{Op: model.OpWrite, Key: []byte("hot"), Expression: model.Expression{Base: model.Register("value"), Delta: 1}},
				model.Instruction{Op: model.OpReturn, Expression: model.Expression{Base: model.Register("value"), Delta: 1}},
			),
			transaction("fast-successor", 3,
				model.Instruction{Op: model.OpRead, Key: []byte("hot"), Register: "value"},
				model.Instruction{Op: model.OpWrite, Key: []byte("hot"), Expression: model.Expression{Base: model.Register("value"), Delta: 1}},
				model.Instruction{Op: model.OpReturn, Expression: model.Expression{Base: model.Register("value"), Delta: 1}},
			),
		},
	}
	serialState := mustState(t, initial)
	serialResult, _, err := serial.New(nil).ExecuteBlock(
		context.Background(), block, serialState, engineapi.RunConfig{Executors: 1},
	)
	if err != nil {
		t.Fatal(err)
	}
	blockSTMState := mustState(t, initial)
	conflictPolicy := newOrderedConflictPolicy()
	blockSTMResult, trace, err := blockstm.New(nil).ExecuteBlock(
		context.Background(), block, blockSTMState, engineapi.RunConfig{
			Executors: 2,
			Policy:    conflictPolicy,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	assertEquivalent(t, serialResult, blockSTMResult, serialState, blockSTMState)

	failures := make(map[uint64]int)
	replays := make(map[uint64]int)
	for _, record := range trace.Events {
		if record.TxIndex != 1 {
			continue
		}
		switch record.Event {
		case control.EventValidationFail:
			failures[record.Incarnation]++
		case control.EventReplayStart:
			replays[record.Incarnation]++
		}
	}
	if len(replays) == 0 {
		t.Fatalf("reexecution hooks were not observed: trace=%#v", trace.Events)
	}
	if !trace.WorkAvailable || trace.Work.ExecutionAttempts <= uint64(len(block.Transactions)) ||
		trace.Work.ReexecutionAttempts == 0 || trace.Work.ReexecutedExecutionUnits == 0 ||
		trace.Work.DiscardedExecutionUnits != trace.Work.ReexecutedExecutionUnits {
		t.Fatalf("reexecution work was not accounted: %#v", trace.Work)
	}
	for incarnation, count := range replays {
		if incarnation == 0 || failures[incarnation-1] != count {
			t.Fatalf("reexecution hooks were not paired by incarnation: failures=%v replays=%v", failures, replays)
		}
	}
}

// orderedConflictPolicy is a test-only synchronization policy. It pauses the
// predecessor immediately before its first write until the successor has
// completed its stale read. That guarantees a validation failure without
// relying on wall time, compute speed, or goroutine scheduling.
type orderedConflictPolicy struct {
	policy.Policy
	successorRead chan struct{}
	signalOnce    sync.Once
}

func newOrderedConflictPolicy() *orderedConflictPolicy {
	return &orderedConflictPolicy{
		Policy:        fixed.NewBlockSTMPreset(),
		successorRead: make(chan struct{}),
	}
}

func (p *orderedConflictPolicy) BeforeWrite(ctx control.AccessContext) control.AccessDecision {
	switch ctx.TransactionID {
	case "blocked-predecessor":
		<-p.successorRead
	case "fast-successor":
		p.signalOnce.Do(func() { close(p.successorRead) })
	}
	return control.AccessDecision{Mode: control.AccessExecute}
}

func executeBoth(
	t *testing.T,
	block model.Block,
	initial []model.StateEntry,
	workers int,
) (model.BlockResult, model.BlockResult, *memkv.Store, *memkv.Store) {
	t.Helper()
	serialState := mustState(t, initial)
	blockSTMState := mustState(t, initial)
	serialResult, _, err := serial.New(nil).ExecuteBlock(
		context.Background(), block, serialState,
		engineapi.RunConfig{Executors: 1},
	)
	if err != nil {
		t.Fatal(err)
	}
	blockSTMResult, _, err := blockstm.New(nil).ExecuteBlock(
		context.Background(), block, blockSTMState,
		engineapi.RunConfig{Executors: workers},
	)
	if err != nil {
		t.Fatal(err)
	}
	return serialResult, blockSTMResult, serialState, blockSTMState
}

func assertEquivalent(
	t *testing.T,
	serialResult model.BlockResult,
	blockSTMResult model.BlockResult,
	serialState *memkv.Store,
	blockSTMState *memkv.Store,
) {
	t.Helper()
	if !reflect.DeepEqual(serialResult, blockSTMResult) {
		t.Fatalf("canonical results differ:\nserial:   %#v\nblockstm: %#v", serialResult, blockSTMResult)
	}
	if !reflect.DeepEqual(serialState.Snapshot(), blockSTMState.Snapshot()) {
		t.Fatalf("published states differ:\nserial:   %#v\nblockstm: %#v", serialState.Snapshot(), blockSTMState.Snapshot())
	}
}

func adversarialBlock() model.Block {
	return model.Block{
		ID:     "adversarial-block",
		Height: 11,
		Transactions: []model.Transaction{
			transaction("slow-hot", 100_004,
				model.Instruction{Op: model.OpRead, Key: []byte("hot"), Register: "value"},
				model.Instruction{Op: model.OpCompute, ComputeUnits: 100_000},
				model.Instruction{Op: model.OpWrite, Key: []byte("hot"), Expression: model.Expression{Base: model.Register("value"), Delta: 1}},
				model.Instruction{Op: model.OpReturn, Expression: model.Expression{Base: model.Register("value"), Delta: 1}},
			),
			transaction("fast-hot", 3,
				model.Instruction{Op: model.OpRead, Key: []byte("hot"), Register: "value"},
				model.Instruction{Op: model.OpWrite, Key: []byte("hot"), Expression: model.Expression{Base: model.Register("value"), Delta: 1}},
				model.Instruction{Op: model.OpReturn, Expression: model.Expression{Base: model.Register("value"), Delta: 1}},
			),
			transaction("explicit-failure", 2,
				model.Instruction{Op: model.OpWrite, Key: []byte("pending-failure"), Expression: model.Expression{Base: model.Literal(1)}},
				model.Instruction{Op: model.OpFailIf, Condition: model.Condition{Kind: model.ConditionAlways}, ErrorCode: "expected_failure"},
			),
			transaction("delete", 3,
				model.Instruction{Op: model.OpRead, Key: []byte("hot"), Register: "value"},
				model.Instruction{Op: model.OpDelete, Key: []byte("doomed")},
				model.Instruction{Op: model.OpReturn, Expression: model.Expression{Base: model.Register("value")}},
			),
			transaction("invalid-state", 1,
				model.Instruction{Op: model.OpRead, Key: []byte("invalid"), Register: "value"},
			),
			transaction("out-of-gas", 2,
				model.Instruction{Op: model.OpWrite, Key: []byte("pending-oog"), Expression: model.Expression{Base: model.Literal(1)}},
				model.Instruction{Op: model.OpCompute, ComputeUnits: 5},
			),
			transaction("overflow", 2,
				model.Instruction{Op: model.OpRead, Key: []byte("max"), Register: "value"},
				model.Instruction{Op: model.OpWrite, Key: []byte("max"), Expression: model.Expression{Base: model.Register("value"), Delta: 1}},
			),
			transaction("branch", 3,
				model.Instruction{Op: model.OpRead, Key: []byte("missing"), Register: "value"},
				model.Instruction{Op: model.OpJumpIf, Condition: model.Condition{Kind: model.ConditionNotExists, Register: "value"}, Target: 3},
				model.Instruction{Op: model.OpFailIf, Condition: model.Condition{Kind: model.ConditionAlways}, ErrorCode: "wrong_branch"},
				model.Instruction{Op: model.OpReturn, Expression: model.Expression{Base: model.Literal(7)}},
			),
		},
	}
}

func transaction(id string, maxUnits uint64, instructions ...model.Instruction) model.Transaction {
	for index := range instructions {
		instructions[index].ID = fmt.Sprintf("%s/op-%d", id, index)
	}
	return model.Transaction{
		ID:       id,
		MaxUnits: maxUnits,
		Program:  model.Program{Instructions: instructions},
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

func testName(seed int64, workers int) string {
	return fmt.Sprintf("seed-%d-workers-%d", seed, workers)
}
