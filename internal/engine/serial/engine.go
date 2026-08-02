package serial

import (
	"context"

	"github.com/crypto-org-chain/go-block-stm/internal/control"
	engineapi "github.com/crypto-org-chain/go-block-stm/internal/engine"
	"github.com/crypto-org-chain/go-block-stm/internal/model"
	"github.com/crypto-org-chain/go-block-stm/internal/policy"
	"github.com/crypto-org-chain/go-block-stm/internal/policy/fixed"
	"github.com/crypto-org-chain/go-block-stm/internal/runtime/flat"
	"github.com/crypto-org-chain/go-block-stm/internal/state"
	"github.com/crypto-org-chain/go-block-stm/internal/state/memkv"
)

var (
	ErrNilState       = engineapi.ErrNilState
	ErrMissingBlockID = engineapi.ErrMissingBlockID
)

const engineName = "serial"

type Engine struct {
	runtime      *flat.Runtime
	capabilities control.Capabilities
}

var _ engineapi.Engine = (*Engine)(nil)

func New(runtime *flat.Runtime) *Engine {
	if runtime == nil {
		runtime = flat.New()
	}
	return &Engine{
		runtime:      runtime,
		capabilities: serialCapabilities(),
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

// ExecuteBlock runs transactions in preset order on a cloned state. Semantic
// transaction failures are recorded and execution continues. Infrastructure
// cancellation/error returns without publishing any partial block state.
func (e *Engine) ExecuteBlock(
	ctx context.Context,
	block model.Block,
	storage state.Store,
	config engineapi.RunConfig,
) (model.BlockResult, control.Trace, error) {
	if err := engineapi.ValidateBlock(block, storage); err != nil {
		return model.BlockResult{}, control.Trace{Engine: engineName}, err
	}
	if config.Executors != 0 && config.Executors != 1 {
		return model.BlockResult{}, control.Trace{Engine: engineName}, engineapi.ErrInvalidWorkers
	}
	if config.MaxSpeculativeInflight < 0 || config.MaxSpeculativeInflight > 1 {
		return model.BlockResult{}, control.Trace{Engine: engineName}, engineapi.ErrInvalidSpeculationLimit
	}
	if err := ctx.Err(); err != nil {
		return model.BlockResult{}, control.Trace{Engine: engineName}, err
	}

	selectedPolicy := config.Policy
	if selectedPolicy == nil {
		selectedPolicy = fixed.NewSerialPreset()
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
	if err := dispatcher.Err(); err != nil {
		return model.BlockResult{}, trace(), err
	}

	working, err := memkv.FromEntries(storage.Snapshot())
	if err != nil {
		return model.BlockResult{}, trace(), err
	}
	result := model.BlockResult{
		BlockID:      block.ID,
		Height:       block.Height,
		Transactions: make([]model.TxResult, 0, len(block.Transactions)),
	}

	for index, transaction := range block.Transactions {
		if err := ctx.Err(); err != nil {
			return model.BlockResult{}, trace(), err
		}
		txContext := control.TxContext{
			BlockID:       block.ID,
			TransactionID: transaction.ID,
			TxIndex:       uint64(index),
			Incarnation:   0,
		}
		admission := dispatcher.OnTxAdmit(txContext)
		if admission.Lane != control.LaneSerial {
			return model.BlockResult{}, trace(), engineapi.Unsupported(control.EventTxAdmit, string(admission.Lane))
		}
		txContext.Ordinal = 1
		ready := dispatcher.OnTxReady(txContext)
		if ready.Lane != control.LaneSerial {
			return model.BlockResult{}, trace(), engineapi.Unsupported(control.EventTxReady, string(ready.Lane))
		}
		txContext.Ordinal = 2
		dispatcher.OnTaskReady(control.TaskContext{TxContext: txContext, Kind: "execution"})
		if err := dispatcher.Err(); err != nil {
			return model.BlockResult{}, trace(), err
		}

		view := state.NewOverlay(working)
		txResult := e.runtime.ExecuteWithHooks(ctx, txContext, transaction, view, dispatcher)
		validationContext := txContext
		validationContext.Ordinal = ^uint64(0) - 1
		dispatcher.OnValidationPoint(control.ValidationContext{
			TxContext: validationContext,
			Kind:      control.ValidationTxEnd,
			TargetID:  transaction.ID,
		})
		if err := dispatcher.Err(); err != nil {
			return model.BlockResult{}, trace(), err
		}
		if txResult.Status == model.TxStatusCancelled {
			if err := ctx.Err(); err != nil {
				return model.BlockResult{}, trace(), err
			}
			return model.BlockResult{}, trace(), context.Canceled
		}
		if txResult.Status == model.TxStatusSuccess {
			view.CommitTo(working)
		}
		result.Transactions = append(result.Transactions, txResult)
	}

	if err := ctx.Err(); err != nil {
		return model.BlockResult{}, trace(), err
	}
	result.FinalState = working.Snapshot()
	if !config.OmitResultDigest {
		result.Digest = model.CanonicalDigest(result)
	}
	if err := storage.Replace(result.FinalState); err != nil {
		return model.BlockResult{}, trace(), err
	}
	finalTrace := trace()
	if traceMode != control.TraceOff {
		finalTrace.WorkAvailable = true
		finalTrace.Work.ExecutionAttempts = uint64(len(result.Transactions))
		finalTrace.Work.SpeculationLimit = 1
		finalTrace.Work.SpeculationTelemetryAvailable = true
		if len(result.Transactions) > 0 {
			finalTrace.Work.PeakSpeculativeInflight = 1
		}
		for _, transaction := range result.Transactions {
			finalTrace.Work.UsefulExecutionUnits += transaction.UnitsUsed
		}
	}
	return result, finalTrace, nil
}

func serialCapabilities() control.Capabilities {
	supported := map[control.Event]bool{
		control.EventEpochStart:  true,
		control.EventBlockReady:  true,
		control.EventTxAdmit:     true,
		control.EventTxReady:     true,
		control.EventTaskReady:   true,
		control.EventBranch:      true,
		control.EventBeforeRead:  true,
		control.EventBeforeWrite: true,
		control.EventTxEnd:       true,
	}
	reasons := make(map[control.Event]string)
	for _, descriptor := range control.EventRegistry() {
		if !supported[descriptor.Event] {
			reasons[descriptor.Event] = "event is outside the flat serial execution path"
		}
	}
	capabilities, err := control.NewCapabilities(engineName, supported, reasons)
	if err != nil {
		panic(err)
	}
	return capabilities
}
