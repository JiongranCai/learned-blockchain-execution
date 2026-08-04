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
	ErrInvalidProgramShape      = errors.New("unsupported synthetic program_shape")
	ErrBranchKeySpace           = errors.New("state_dependent_branch requires initial_keys greater than key_space")
)

const ProgramShapeStateDependentBranch = "state_dependent_branch"

type Config struct {
	Seed                 int64  `json:"seed"`
	InitialKeys          int    `json:"initial_keys"`
	KeySpace             int    `json:"key_space"`
	BlockCount           int    `json:"block_count"`
	TransactionsPerBlock int    `json:"transactions_per_block"`
	MaxComputeUnits      uint64 `json:"max_compute_units"`
	TransactionMaxUnits  uint64 `json:"transaction_max_units"`
	FailureEvery         int    `json:"failure_every"`
	ProgramShape         string `json:"program_shape,omitempty"`
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

	initialValues := make([]int64, config.InitialKeys)
	for i := 0; i < config.InitialKeys; i++ {
		initialValues[i] = int64(rng.Intn(10_000))
		artifact.InitialState = append(artifact.InitialState, model.StateEntry{
			Key:   stateKey(i),
			Value: flat.EncodeInt64(initialValues[i]),
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

			willFail := config.FailureEvery > 0 && (globalTransaction+1)%config.FailureEvery == 0
			instructions, branchTruth := flatProgram(readKey, writeKey, delta, computeUnits)
			if config.ProgramShape == ProgramShapeStateDependentBranch {
				alternateReadKey := stateKey(rng.Intn(config.KeySpace))
				selectorIndex := config.KeySpace + rng.Intn(config.InitialKeys-config.KeySpace)
				instructions, branchTruth = stateDependentBranchProgram(
					stateKey(selectorIndex),
					initialValues[selectorIndex] < 5_000,
					readKey,
					alternateReadKey,
					writeKey,
					delta,
					computeUnits,
				)
			}
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

			expectedStatus := model.TxStatusSuccess
			if willFail {
				expectedStatus = model.TxStatusFailed
			}
			actualIndices := branchTruth.actualInstructionIndices(willFail, len(instructions))
			operationPath := make([]string, 0, len(actualIndices))
			for _, instructionIndex := range actualIndices {
				operationPath = append(operationPath, instructions[instructionIndex].ID)
			}
			accesses := branchTruth.accesses(instructions)
			branches := branchTruth.branches(instructions)
			artifact.GroundTruth = append(artifact.GroundTruth, workload.TransactionGroundTruth{
				TransactionID:  transactionID,
				ExpectedStatus: expectedStatus,
				OperationPath:  operationPath,
				Accesses:       accesses,
				Branches:       branches,
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
	if config.ProgramShape != "" && config.ProgramShape != ProgramShapeStateDependentBranch {
		return ErrInvalidProgramShape
	}
	if config.ProgramShape == ProgramShapeStateDependentBranch && config.InitialKeys <= config.KeySpace {
		return ErrBranchKeySpace
	}
	// Int63n needs MaxComputeUnits+1 to remain a positive int64. This also
	// leaves ample room for the fixed per-transaction instruction costs.
	if config.MaxComputeUnits >= uint64(math.MaxInt64) {
		return ErrInvalidTransactionBudget
	}
	minimumUnits := config.MaxComputeUnits + 4
	if config.ProgramShape == ProgramShapeStateDependentBranch {
		minimumUnits = config.MaxComputeUnits + 7
	}
	if config.TransactionMaxUnits < minimumUnits {
		return ErrInvalidTransactionBudget
	}
	return nil
}

type programTruth struct {
	branching         bool
	branchTaken       bool
	selectorRead      int
	untakenRead       int
	takenRead         int
	write             int
	conditionalJump   int
	unconditionalJump int
}

func flatProgram(readKey, writeKey []byte, delta int64, computeUnits uint64) ([]model.Instruction, programTruth) {
	return []model.Instruction{
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
	}, programTruth{selectorRead: 0, write: 2}
}

func stateDependentBranchProgram(
	selectorKey []byte,
	branchTaken bool,
	untakenReadKey []byte,
	takenReadKey []byte,
	writeKey []byte,
	delta int64,
	computeUnits uint64,
) ([]model.Instruction, programTruth) {
	return []model.Instruction{
			{Op: model.OpRead, Key: selectorKey, Register: "selector"},
			{
				Op: model.OpJumpIf,
				Condition: model.Condition{
					Kind:  model.ConditionLess,
					Left:  model.Register("selector"),
					Right: model.Literal(5_000),
				},
				Target: 4,
			},
			{Op: model.OpRead, Key: untakenReadKey, Register: "value"},
			{Op: model.OpJumpIf, Condition: model.Condition{Kind: model.ConditionAlways}, Target: 5},
			{Op: model.OpRead, Key: takenReadKey, Register: "value"},
			{Op: model.OpCompute, ComputeUnits: computeUnits},
			{
				Op:  model.OpWrite,
				Key: writeKey,
				Expression: model.Expression{
					Base:  model.Register("value"),
					Delta: delta,
				},
			},
		}, programTruth{
			branching:         true,
			branchTaken:       branchTaken,
			selectorRead:      0,
			untakenRead:       2,
			takenRead:         4,
			write:             6,
			conditionalJump:   1,
			unconditionalJump: 3,
		}
}

func (truth programTruth) actualInstructionIndices(willFail bool, instructionCount int) []int {
	if !truth.branching {
		count := instructionCount
		if willFail {
			count-- // RETURN is structurally present but unreachable.
		}
		indices := make([]int, count)
		for index := range indices {
			indices[index] = index
		}
		return indices
	}

	indices := []int{truth.selectorRead, truth.conditionalJump}
	if truth.branchTaken {
		indices = append(indices, truth.takenRead)
	} else {
		indices = append(indices, truth.untakenRead, truth.unconditionalJump)
	}
	indices = append(indices, 5, truth.write)
	if willFail {
		indices = append(indices, instructionCount-2)
	} else {
		indices = append(indices, instructionCount-1)
	}
	return indices
}

func (truth programTruth) accesses(instructions []model.Instruction) []workload.GroundTruthAccess {
	readIndex := truth.selectorRead
	accesses := make([]workload.GroundTruthAccess, 0, 3)
	if truth.branching {
		accesses = append(accesses, workload.GroundTruthAccess{
			OperationID: instructions[truth.selectorRead].ID,
			Mode:        workload.AccessRead,
			Key:         cloneBytes(instructions[truth.selectorRead].Key),
		})
		if truth.branchTaken {
			readIndex = truth.takenRead
		} else {
			readIndex = truth.untakenRead
		}
	}
	accesses = append(accesses,
		workload.GroundTruthAccess{
			OperationID: instructions[readIndex].ID,
			Mode:        workload.AccessRead,
			Key:         cloneBytes(instructions[readIndex].Key),
		},
		workload.GroundTruthAccess{
			OperationID: instructions[truth.write].ID,
			Mode:        workload.AccessWrite,
			Key:         cloneBytes(instructions[truth.write].Key),
		},
	)
	return accesses
}

func (truth programTruth) branches(instructions []model.Instruction) []workload.BranchOutcome {
	if !truth.branching {
		return make([]workload.BranchOutcome, 0)
	}
	branches := []workload.BranchOutcome{{
		BranchID: instructions[truth.conditionalJump].ID,
		Taken:    truth.branchTaken,
		Target:   instructions[truth.conditionalJump].Target,
	}}
	if !truth.branchTaken {
		branches = append(branches, workload.BranchOutcome{
			BranchID: instructions[truth.unconditionalJump].ID,
			Taken:    true,
			Target:   instructions[truth.unconditionalJump].Target,
		})
	}
	return branches
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
