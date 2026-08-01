package synthetic

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/rand"

	"github.com/crypto-org-chain/go-block-stm/internal/model"
	"github.com/crypto-org-chain/go-block-stm/internal/runtime/flat"
	"github.com/crypto-org-chain/go-block-stm/internal/workload"
)

const (
	GeneratorName    = "synthetic"
	GeneratorVersion = "synthetic-v1"
)

var (
	ErrInvalidInitialKeys       = errors.New("initial_keys must be positive")
	ErrInvalidKeySpace          = errors.New("key_space must be in [1, initial_keys]")
	ErrInvalidBlockCount        = errors.New("block_count must be positive")
	ErrInvalidTransactions      = errors.New("transactions_per_block must be positive")
	ErrInvalidTransactionBudget = errors.New("transaction_max_units is too small")
	ErrInvalidFailureInterval   = errors.New("failure_every must be non-negative")
)

type Config struct {
	Seed                 int64  `json:"seed"`
	InitialKeys          int    `json:"initial_keys"`
	KeySpace             int    `json:"key_space"`
	BlockCount           int    `json:"block_count"`
	TransactionsPerBlock int    `json:"transactions_per_block"`
	MaxComputeUnits      uint64 `json:"max_compute_units"`
	TransactionMaxUnits  uint64 `json:"transaction_max_units"`
	FailureEvery         int    `json:"failure_every"`
}

func Generate(config Config) (workload.Artifact, error) {
	if err := validateConfig(config); err != nil {
		return workload.Artifact{}, err
	}
	configDescriptor, err := json.Marshal(config)
	if err != nil {
		return workload.Artifact{}, err
	}

	rng := rand.New(rand.NewSource(config.Seed))
	artifact := workload.Artifact{
		SchemaVersion: workload.ArtifactSchemaVersion,
		Generator: workload.GeneratorDescriptor{
			Name:    GeneratorName,
			Version: GeneratorVersion,
			Seed:    config.Seed,
			Config:  configDescriptor,
		},
		InitialState:           make([]model.StateEntry, 0, config.InitialKeys),
		OrderedBlocks:          make([]model.Block, 0, config.BlockCount),
		LogicalArrivalSchedule: make([]workload.LogicalArrival, 0),
		GroundTruth:            make([]workload.TransactionGroundTruth, 0),
		EngineVisibleMetadata:  make([]workload.MetadataRecord, 0),
	}

	for i := 0; i < config.InitialKeys; i++ {
		artifact.InitialState = append(artifact.InitialState, model.StateEntry{
			Key:   stateKey(i),
			Value: flat.EncodeInt64(int64(rng.Intn(10_000))),
		})
	}

	globalTransaction := 0
	for blockIndex := 0; blockIndex < config.BlockCount; blockIndex++ {
		block := model.Block{
			ID:           fmt.Sprintf("block-%06d", blockIndex),
			Height:       uint64(blockIndex),
			Transactions: make([]model.Transaction, 0, config.TransactionsPerBlock),
		}
		for transactionIndex := 0; transactionIndex < config.TransactionsPerBlock; transactionIndex++ {
			transactionID := fmt.Sprintf("tx-%06d-%06d", blockIndex, transactionIndex)
			readKey := stateKey(rng.Intn(config.KeySpace))
			writeKey := stateKey(rng.Intn(config.KeySpace))
			delta := int64(rng.Intn(11) - 5)
			computeUnits := uint64(0)
			if config.MaxComputeUnits > 0 {
				computeUnits = uint64(rng.Int63n(int64(config.MaxComputeUnits + 1)))
			}

			instructions := []model.Instruction{
				{Op: model.OpRead, Key: readKey, Register: "value"},
				{Op: model.OpCompute, ComputeUnits: computeUnits},
				{
					Op:  model.OpWrite,
					Key: writeKey,
					Expression: model.Expression{
						Base:  model.Register("value"),
						Delta: delta,
					},
				},
			}
			willFail := config.FailureEvery > 0 && (globalTransaction+1)%config.FailureEvery == 0
			if willFail {
				instructions = append(instructions, model.Instruction{
					Op:        model.OpFailIf,
					Condition: model.Condition{Kind: model.ConditionAlways},
					ErrorCode: "synthetic_failure",
				})
			}
			instructions = append(instructions, model.Instruction{
				Op: model.OpReturn,
				Expression: model.Expression{
					Base: model.Register("value"),
				},
			})
			for instructionIndex := range instructions {
				instructions[instructionIndex].ID = operationID(blockIndex, transactionIndex, instructionIndex)
			}

			block.Transactions = append(block.Transactions, model.Transaction{
				ID:       transactionID,
				MaxUnits: config.TransactionMaxUnits,
				Program:  model.Program{Instructions: instructions},
			})

			actualOperationCount := len(instructions)
			expectedStatus := model.TxStatusSuccess
			if willFail {
				actualOperationCount-- // RETURN is structurally present but unreachable.
				expectedStatus = model.TxStatusFailed
			}
			operationPath := make([]string, 0, actualOperationCount)
			for _, instruction := range instructions[:actualOperationCount] {
				operationPath = append(operationPath, instruction.ID)
			}
			artifact.GroundTruth = append(artifact.GroundTruth, workload.TransactionGroundTruth{
				TransactionID:  transactionID,
				ExpectedStatus: expectedStatus,
				OperationPath:  operationPath,
				Accesses: []workload.GroundTruthAccess{
					{OperationID: instructions[0].ID, Mode: workload.AccessRead, Key: cloneBytes(readKey)},
					{OperationID: instructions[2].ID, Mode: workload.AccessWrite, Key: cloneBytes(writeKey)},
				},
				Branches: make([]workload.BranchOutcome, 0),
			})
			artifact.LogicalArrivalSchedule = append(artifact.LogicalArrivalSchedule, workload.LogicalArrival{
				Sequence:      uint64(globalTransaction),
				LogicalTime:   uint64(globalTransaction),
				BlockID:       block.ID,
				TransactionID: transactionID,
			})
			globalTransaction++
		}
		artifact.OrderedBlocks = append(artifact.OrderedBlocks, block)
	}

	if err := artifact.Seal(); err != nil {
		return workload.Artifact{}, err
	}
	return artifact, nil
}

func validateConfig(config Config) error {
	if config.InitialKeys <= 0 {
		return ErrInvalidInitialKeys
	}
	if config.KeySpace <= 0 || config.KeySpace > config.InitialKeys {
		return ErrInvalidKeySpace
	}
	if config.BlockCount <= 0 {
		return ErrInvalidBlockCount
	}
	if config.TransactionsPerBlock <= 0 {
		return ErrInvalidTransactions
	}
	if config.FailureEvery < 0 {
		return ErrInvalidFailureInterval
	}
	// Int63n needs MaxComputeUnits+1 to remain a positive int64. This also
	// leaves ample room for the fixed per-transaction instruction costs.
	if config.MaxComputeUnits >= uint64(math.MaxInt64) {
		return ErrInvalidTransactionBudget
	}
	minimumUnits := config.MaxComputeUnits + 4
	if config.TransactionMaxUnits < minimumUnits {
		return ErrInvalidTransactionBudget
	}
	return nil
}

func stateKey(index int) []byte {
	return []byte(fmt.Sprintf("key-%08d", index))
}

func operationID(blockIndex, transactionIndex, instructionIndex int) string {
	return fmt.Sprintf("op-%06d-%06d-%03d", blockIndex, transactionIndex, instructionIndex)
}

func cloneBytes(value []byte) []byte {
	return append([]byte(nil), value...)
}
