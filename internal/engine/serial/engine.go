package serial

import (
	"context"
	"errors"
	"fmt"

	"github.com/crypto-org-chain/go-block-stm/internal/model"
	"github.com/crypto-org-chain/go-block-stm/internal/runtime/flat"
	"github.com/crypto-org-chain/go-block-stm/internal/state"
	"github.com/crypto-org-chain/go-block-stm/internal/state/memkv"
)

var (
	ErrNilState       = errors.New("serial engine requires a non-nil state")
	ErrMissingBlockID = errors.New("block id is required")
)

type Engine struct {
	runtime *flat.Runtime
}

func New(runtime *flat.Runtime) *Engine {
	if runtime == nil {
		runtime = flat.New()
	}
	return &Engine{runtime: runtime}
}

// ExecuteBlock runs transactions in preset order on a cloned state. Semantic
// transaction failures are recorded and execution continues. Infrastructure
// cancellation/error returns without publishing any partial block state.
func (e *Engine) ExecuteBlock(
	ctx context.Context,
	block model.Block,
	storage *memkv.Store,
) (model.BlockResult, error) {
	if storage == nil {
		return model.BlockResult{}, ErrNilState
	}
	if block.ID == "" {
		return model.BlockResult{}, ErrMissingBlockID
	}
	if err := validateTransactionIDs(block.Transactions); err != nil {
		return model.BlockResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return model.BlockResult{}, err
	}

	working := storage.Clone()
	result := model.BlockResult{
		BlockID:      block.ID,
		Height:       block.Height,
		Transactions: make([]model.TxResult, 0, len(block.Transactions)),
	}

	for index, transaction := range block.Transactions {
		if err := ctx.Err(); err != nil {
			return model.BlockResult{}, err
		}

		view := state.NewOverlay(working)
		txResult := e.runtime.Execute(ctx, uint64(index), transaction, view)
		if txResult.Status == model.TxStatusCancelled {
			if err := ctx.Err(); err != nil {
				return model.BlockResult{}, err
			}
			return model.BlockResult{}, context.Canceled
		}
		if txResult.Status == model.TxStatusSuccess {
			view.CommitTo(working)
		}
		result.Transactions = append(result.Transactions, txResult)
	}

	if err := ctx.Err(); err != nil {
		return model.BlockResult{}, err
	}
	result.FinalState = working.Snapshot()
	result.Digest = model.CanonicalDigest(result)
	storage.ReplaceFrom(working)
	return result, nil
}

func validateTransactionIDs(transactions []model.Transaction) error {
	seen := make(map[string]struct{}, len(transactions))
	for index, transaction := range transactions {
		if transaction.ID == "" {
			return fmt.Errorf("transaction %d has no id", index)
		}
		if _, exists := seen[transaction.ID]; exists {
			return fmt.Errorf("duplicate transaction id %q", transaction.ID)
		}
		seen[transaction.ID] = struct{}{}
	}
	return nil
}
