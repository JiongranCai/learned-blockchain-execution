package blockstm

import (
	"context"
	"fmt"
	"sync"

	storetypes "cosmossdk.io/store/types"
	kernel "github.com/crypto-org-chain/go-block-stm"
	"github.com/crypto-org-chain/go-block-stm/internal/control"
	engineapi "github.com/crypto-org-chain/go-block-stm/internal/engine"
	"github.com/crypto-org-chain/go-block-stm/internal/model"
	"github.com/crypto-org-chain/go-block-stm/internal/policy"
	"github.com/crypto-org-chain/go-block-stm/internal/policy/fixed"
	"github.com/crypto-org-chain/go-block-stm/internal/runtime/flat"
	"github.com/crypto-org-chain/go-block-stm/internal/state"
	"github.com/crypto-org-chain/go-block-stm/internal/state/memkv"
)

const (
	engineName   = "blockstm"
	adapterStore = "flat-runtime"
)

type Engine struct {
	runtime      *flat.Runtime
	storeKey     *storetypes.KVStoreKey
	stores       map[storetypes.StoreKey]int
	capabilities control.Capabilities
}

var _ engineapi.Engine = (*Engine)(nil)

func New(runtime *flat.Runtime) *Engine {
	if runtime == nil {
		runtime = flat.New()
	}
	storeKey := storetypes.NewKVStoreKey(adapterStore)
	return &Engine{
		runtime:      runtime,
		storeKey:     storeKey,
		stores:       map[storetypes.StoreKey]int{storeKey: 0},
		capabilities: blockSTMCapabilities(),
	}
}

func (e *Engine) Name() string {
	return engineName
}

func (e *Engine) Capabilities() control.Capabilities {
	capabilities := e.capabilities
	capabilities.Events = append([]control.EventCapability(nil), e.capabilities.Events...)
	return capabilities
}

func (e *Engine) ExecuteBlock(
	ctx context.Context,
	block model.Block,
	storage state.Store,
	config engineapi.RunConfig,
) (model.BlockResult, control.Trace, error) {
	if err := engineapi.ValidateBlock(block, storage); err != nil {
		return model.BlockResult{}, control.Trace{Engine: engineName}, err
	}
	if config.Executors < 0 {
		return model.BlockResult{}, control.Trace{Engine: engineName}, engineapi.ErrInvalidWorkers
	}
	if config.MaxSpeculativeInflight < 0 {
		return model.BlockResult{}, control.Trace{Engine: engineName}, engineapi.ErrInvalidSpeculationLimit
	}
	if err := ctx.Err(); err != nil {
		return model.BlockResult{}, control.Trace{Engine: engineName}, err
	}

	selectedPolicy := config.Policy
	if selectedPolicy == nil {
		selectedPolicy = fixed.NewBlockSTMPreset()
	}
	traceMode, err := engineapi.EffectiveTraceMode(config)
	if err != nil {
		return model.BlockResult{}, control.Trace{Engine: engineName}, err
	}
	dispatcher, err := policy.NewDispatcherWithTrace(selectedPolicy, traceMode)
	if err != nil {
		return model.BlockResult{}, control.Trace{Engine: engineName}, err
	}
	trace := func() control.Trace { return dispatcher.Trace(engineName) }

	dispatcher.OnEpochStart(control.EpochContext{EpochID: engineapi.EpochID(config, block)})
	dispatcher.OnBlockReady(control.BlockContext{
		BlockID:          block.ID,
		Height:           block.Height,
		TransactionCount: len(block.Transactions),
	})
	for index, transaction := range block.Transactions {
		admission := dispatcher.OnTxAdmit(control.TxContext{
			BlockID:       block.ID,
			TransactionID: transaction.ID,
			TxIndex:       uint64(index),
		})
		if admission.Lane != control.LaneOptimistic {
			return model.BlockResult{}, trace(), engineapi.Unsupported(control.EventTxAdmit, string(admission.Lane))
		}
	}
	if err := dispatcher.Err(); err != nil {
		return model.BlockResult{}, trace(), err
	}

	initial, err := memkv.FromEntries(storage.Snapshot())
	if err != nil {
		return model.BlockResult{}, trace(), err
	}
	kernelStorage := newKernelStorage(e.stores, e.storeKey, initial.Snapshot())
	slots := make([]resultSlot, len(block.Transactions))
	executionCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	executionErrors := newExecutionErrors()

	txExecutor := func(index kernel.TxnIndex, multiStore kernel.MultiStore) {
		transactionIndex := int(index)
		if transactionIndex < 0 || transactionIndex >= len(block.Transactions) {
			executionErrors.set(fmt.Errorf("kernel returned invalid transaction index %d", index))
			cancel()
			return
		}
		transaction := block.Transactions[transactionIndex]
		incarnation := slots[transactionIndex].beginExecution()
		txContext := control.TxContext{
			BlockID:       block.ID,
			TransactionID: transaction.ID,
			TxIndex:       uint64(transactionIndex),
			Incarnation:   incarnation,
		}

		if incarnation > 0 {
			failedContext := txContext
			failedContext.Incarnation = incarnation - 1
			failedContext.Ordinal = ^uint64(0)
			dispatcher.OnValidationFail(control.FailureContext{
				TxContext: failedContext,
				Reason:    "read_set_changed",
			})
			txContext.Ordinal = 1
			dispatcher.OnReplayStart(control.ReplayContext{
				TxContext: txContext,
				Reason:    "blockstm_reexecution",
			})
		}
		txContext.Ordinal = 2
		ready := dispatcher.OnTxReady(txContext)
		if ready.Lane != control.LaneOptimistic {
			executionErrors.set(engineapi.Unsupported(control.EventTxReady, string(ready.Lane)))
			cancel()
			return
		}
		txContext.Ordinal = 3
		dispatcher.OnTaskReady(control.TaskContext{TxContext: txContext, Kind: "execution"})
		if err := dispatcher.Err(); err != nil {
			executionErrors.set(err)
			cancel()
			return
		}

		adapter := framedState{store: multiStore.GetKVStore(e.storeKey)}
		view := state.NewOverlay(adapter)
		txResult := e.runtime.ExecuteWithHooks(executionCtx, txContext, transaction, view, dispatcher)
		validationContext := txContext
		validationContext.Ordinal = ^uint64(0) - 1
		dispatcher.OnValidationPoint(control.ValidationContext{
			TxContext: validationContext,
			Kind:      control.ValidationTxEnd,
			TargetID:  transaction.ID,
		})
		if err := dispatcher.Err(); err != nil {
			executionErrors.set(err)
			cancel()
			return
		}
		if txResult.Status == model.TxStatusSuccess {
			view.CommitTo(adapter)
		}
		slots[transactionIndex].store(incarnation, txResult, traceMode != control.TraceOff)
	}

	speculation, err := kernel.ExecuteBlockWithMaxSpeculativeInflight(
		executionCtx,
		len(block.Transactions),
		e.stores,
		kernelStorage,
		config.Executors,
		config.MaxSpeculativeInflight,
		txExecutor,
	)
	if executionErr := executionErrors.get(); executionErr != nil {
		return model.BlockResult{}, trace(), executionErr
	}
	if err != nil {
		return model.BlockResult{}, trace(), err
	}
	if err := dispatcher.Err(); err != nil {
		return model.BlockResult{}, trace(), err
	}

	result := model.BlockResult{
		BlockID:      block.ID,
		Height:       block.Height,
		Transactions: make([]model.TxResult, len(block.Transactions)),
	}
	for index := range slots {
		txResult, ok := slots[index].load()
		if !ok {
			return model.BlockResult{}, trace(), fmt.Errorf("transaction %d has no final Block-STM result", index)
		}
		if txResult.Status == model.TxStatusCancelled {
			if err := ctx.Err(); err != nil {
				return model.BlockResult{}, trace(), err
			}
			return model.BlockResult{}, trace(), context.Canceled
		}
		result.Transactions[index] = txResult
	}
	result.FinalState, err = snapshotKernelStorage(kernelStorage, e.storeKey)
	if err != nil {
		return model.BlockResult{}, trace(), err
	}
	if !config.OmitResultDigest {
		result.Digest = model.CanonicalDigest(result)
	}
	if err := storage.Replace(result.FinalState); err != nil {
		return model.BlockResult{}, trace(), err
	}
	finalTrace := trace()
	if traceMode != control.TraceOff {
		finalTrace.WorkAvailable = true
		finalTrace.Work.SpeculationLimit = speculation.EffectiveLimit
		finalTrace.Work.SpeculationLimitApplied = speculation.LimitApplied
		finalTrace.Work.SpeculationTelemetryAvailable = speculation.TelemetryAvailable
		finalTrace.Work.PeakSpeculativeInflight = speculation.PeakInflight
		finalTrace.Work.AdmissionStallEvents = speculation.AdmissionStallEvents
		finalTrace.Work.AdmissionStallNS = speculation.AdmissionStallNS
		for index := range slots {
			addWorkCounters(&finalTrace.Work, slots[index].work())
		}
	}
	return result, finalTrace, nil
}

type resultSlot struct {
	mu           sync.Mutex
	executions   uint64
	result       model.TxResult
	ready        bool
	attemptUnits []uint64
}

func (s *resultSlot) beginExecution() uint64 {
	s.mu.Lock()
	incarnation := s.executions
	s.executions++
	s.mu.Unlock()
	return incarnation
}

func (s *resultSlot) store(incarnation uint64, result model.TxResult, captureWork bool) {
	s.mu.Lock()
	s.result = result
	s.ready = true
	if captureWork {
		for uint64(len(s.attemptUnits)) <= incarnation {
			s.attemptUnits = append(s.attemptUnits, 0)
		}
		s.attemptUnits[incarnation] = result.UnitsUsed
	}
	s.mu.Unlock()
}

func (s *resultSlot) load() (model.TxResult, bool) {
	s.mu.Lock()
	result, ready := s.result, s.ready
	s.mu.Unlock()
	return result, ready
}

func (s *resultSlot) work() control.WorkCounters {
	s.mu.Lock()
	attempts := append([]uint64(nil), s.attemptUnits...)
	s.mu.Unlock()
	if len(attempts) == 0 {
		return control.WorkCounters{}
	}
	work := control.WorkCounters{
		ExecutionAttempts:    uint64(len(attempts)),
		ReexecutionAttempts:  uint64(len(attempts) - 1),
		UsefulExecutionUnits: attempts[len(attempts)-1],
	}
	for _, units := range attempts[:len(attempts)-1] {
		work.ReexecutedExecutionUnits += units
		work.DiscardedExecutionUnits += units
	}
	return work
}

func addWorkCounters(target *control.WorkCounters, source control.WorkCounters) {
	target.ExecutionAttempts += source.ExecutionAttempts
	target.ReexecutionAttempts += source.ReexecutionAttempts
	target.UsefulExecutionUnits += source.UsefulExecutionUnits
	target.ReexecutedExecutionUnits += source.ReexecutedExecutionUnits
	target.DiscardedExecutionUnits += source.DiscardedExecutionUnits
}

type executionErrors struct {
	mu  sync.Mutex
	err error
}

func newExecutionErrors() *executionErrors {
	return &executionErrors{}
}

func (e *executionErrors) set(err error) {
	if err == nil {
		return
	}
	e.mu.Lock()
	if e.err == nil {
		e.err = err
	}
	e.mu.Unlock()
}

func (e *executionErrors) get() error {
	e.mu.Lock()
	err := e.err
	e.mu.Unlock()
	return err
}

func blockSTMCapabilities() control.Capabilities {
	supported := map[control.Event]bool{
		control.EventEpochStart:     true,
		control.EventBlockReady:     true,
		control.EventTxAdmit:        true,
		control.EventTxReady:        true,
		control.EventTaskReady:      true,
		control.EventBranch:         true,
		control.EventBeforeRead:     true,
		control.EventBeforeWrite:    true,
		control.EventTxEnd:          true,
		control.EventValidationFail: true,
		control.EventReplayStart:    true,
	}
	reasons := make(map[control.Event]string)
	for _, descriptor := range control.EventRegistry() {
		if supported[descriptor.Event] {
			continue
		}
		switch descriptor.Event {
		case control.EventReadEstimate, control.EventConflict:
			reasons[descriptor.Event] = "frozen TxExecutor API does not expose internal MVMemory read/validation callbacks"
		case control.EventWorkerIdle, control.EventQueuePressure:
			reasons[descriptor.Event] = "frozen scheduler does not expose worker or queue callbacks"
		case control.EventRetryLimit:
			reasons[descriptor.Event] = "frozen scheduler has no retry-limit callback"
		default:
			reasons[descriptor.Event] = "event requires nested runtime or an explicit validation point"
		}
	}
	capabilities, err := control.NewCapabilities(engineName, supported, reasons)
	if err != nil {
		panic(err)
	}
	return capabilities
}
