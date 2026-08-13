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
	GeneratorName      = "synthetic"
	GeneratorVersion   = "synthetic-v1"
	GeneratorVersionV2 = "synthetic-v2"
	GeneratorVersionV3 = "synthetic-v3"

	AccessDistributionUniform = "uniform"
	AccessDistributionHotspot = "hotspot"
)

var (
	ErrInvalidInitialKeys          = errors.New("initial_keys must be positive")
	ErrInvalidKeySpace             = errors.New("key_space must be in [1, initial_keys]")
	ErrInvalidBlockCount           = errors.New("block_count must be positive")
	ErrInvalidTransactions         = errors.New("transactions_per_block must be positive")
	ErrInvalidTransactionBudget    = errors.New("transaction_max_units is too small")
	ErrInvalidComputeRange         = errors.New("min_compute_units must not exceed max_compute_units")
	ErrInvalidFailureInterval      = errors.New("failure_every must be non-negative")
	ErrInvalidProgramShape         = errors.New("unsupported synthetic program_shape")
	ErrBranchKeySpace              = errors.New("branching program shapes require initial_keys greater than key_space")
	ErrInvalidAccessDistribution   = errors.New("unsupported access_distribution kind")
	ErrInvalidHotKeyCount          = errors.New("hot_key_count must be in [1, key_space-1]")
	ErrInvalidHotProbability       = errors.New("hot_access_probability must be strictly between 0 and 1")
	ErrInvalidReadWriteCorrelation = errors.New("read_write_same_key_probability must be in [0, 1]")
	ErrInvalidSelectiveReadCount   = errors.New("selective_read_set requires branch_read_candidates in [2, key_space]")
	ErrInvalidStageFanIn           = errors.New("staged_fan_in requires stage_fan_in in [2, transactions_per_block-1]")
	ErrStructuredKeySpace          = errors.New("staged_fan_in requires one key per generated transaction")
	ErrStructuredDistribution      = errors.New("structured program shapes do not accept access_distribution")
	ErrUnexpectedShapeParameter    = errors.New("program shape parameter is set for an incompatible shape")
)

const (
	ProgramShapeStateDependentBranch = "state_dependent_branch"
	ProgramShapeSelectiveReadSet     = "selective_read_set"
	ProgramShapeStagedFanIn          = "staged_fan_in"
)

type Config struct {
	Seed                 int64                     `json:"seed"`
	InitialKeys          int                       `json:"initial_keys"`
	KeySpace             int                       `json:"key_space"`
	BlockCount           int                       `json:"block_count"`
	TransactionsPerBlock int                       `json:"transactions_per_block"`
	MaxComputeUnits      uint64                    `json:"max_compute_units"`
	MinComputeUnits      uint64                    `json:"min_compute_units,omitempty"`
	TransactionMaxUnits  uint64                    `json:"transaction_max_units"`
	FailureEvery         int                       `json:"failure_every"`
	ProgramShape         string                    `json:"program_shape,omitempty"`
	BranchReadCandidates int                       `json:"branch_read_candidates,omitempty"`
	StageFanIn           int                       `json:"stage_fan_in,omitempty"`
	AccessDistribution   *AccessDistributionConfig `json:"access_distribution,omitempty"`
}

type AccessDistributionConfig struct {
	Kind                        string  `json:"kind"`
	HotKeyCount                 int     `json:"hot_key_count,omitempty"`
	HotAccessProbability        float64 `json:"hot_access_probability,omitempty"`
	ReadWriteSameKeyProbability float64 `json:"read_write_same_key_probability,omitempty"`
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
			Version: generatorVersion(config),
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
			readKeyIndex := sampleKeyIndex(rng, config)
			writeKeyIndex, readWriteCorrelated := sampleWriteKeyIndex(rng, config, readKeyIndex)
			readKey := stateKey(readKeyIndex)
			writeKey := stateKey(writeKeyIndex)
			delta := int64(rng.Intn(11) - 5)
			computeUnits := config.MinComputeUnits
			if config.MaxComputeUnits > config.MinComputeUnits {
				width := config.MaxComputeUnits - config.MinComputeUnits + 1
				computeUnits += uint64(rng.Int63n(int64(width)))
			}

			willFail := config.FailureEvery > 0 && (globalTransaction+1)%config.FailureEvery == 0
			instructions, branchTruth := flatProgram(readKey, writeKey, delta, computeUnits)
			switch config.ProgramShape {
			case ProgramShapeStateDependentBranch:
				alternateReadKeyIndex := sampleKeyIndex(rng, config)
				alternateReadKey := stateKey(alternateReadKeyIndex)
				selectorIndex := config.KeySpace + rng.Intn(config.InitialKeys-config.KeySpace)
				branchTaken := initialValues[selectorIndex] < 5_000
				if readWriteCorrelated && branchTaken {
					writeKey = stateKey(alternateReadKeyIndex)
				}
				instructions, branchTruth = stateDependentBranchProgram(
					stateKey(selectorIndex),
					branchTaken,
					readKey,
					alternateReadKey,
					writeKey,
					delta,
					computeUnits,
				)
			case ProgramShapeSelectiveReadSet:
				selectorIndex := config.KeySpace + globalTransaction%(config.InitialKeys-config.KeySpace)
				candidateReadKeys := make([][]byte, config.BranchReadCandidates)
				for candidateIndex := range candidateReadKeys {
					candidateReadKeys[candidateIndex] = stateKey(candidateIndex)
				}
				selectedCandidate := int(initialValues[selectorIndex]) * config.BranchReadCandidates / 10_000
				writeKey = candidateReadKeys[selectedCandidate]
				instructions, branchTruth = selectiveReadSetProgram(
					stateKey(selectorIndex),
					initialValues[selectorIndex],
					candidateReadKeys,
					writeKey,
					delta,
					computeUnits,
				)
			case ProgramShapeStagedFanIn:
				instructions, branchTruth = stagedFanInProgram(
					transactionIndex,
					globalTransaction,
					config.StageFanIn,
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
	if config.MinComputeUnits > config.MaxComputeUnits {
		return ErrInvalidComputeRange
	}
	if config.ProgramShape != "" &&
		config.ProgramShape != ProgramShapeStateDependentBranch &&
		config.ProgramShape != ProgramShapeSelectiveReadSet &&
		config.ProgramShape != ProgramShapeStagedFanIn {
		return ErrInvalidProgramShape
	}
	if (config.ProgramShape == ProgramShapeStateDependentBranch || config.ProgramShape == ProgramShapeSelectiveReadSet) &&
		config.InitialKeys <= config.KeySpace {
		return ErrBranchKeySpace
	}
	if config.ProgramShape == ProgramShapeSelectiveReadSet {
		if config.BranchReadCandidates < 2 || config.BranchReadCandidates > config.KeySpace {
			return ErrInvalidSelectiveReadCount
		}
		if config.StageFanIn != 0 {
			return ErrUnexpectedShapeParameter
		}
	} else if config.BranchReadCandidates != 0 {
		return ErrUnexpectedShapeParameter
	}
	if config.ProgramShape == ProgramShapeStagedFanIn {
		if config.StageFanIn < 2 || config.StageFanIn >= config.TransactionsPerBlock {
			return ErrInvalidStageFanIn
		}
		if config.BlockCount > config.KeySpace/config.TransactionsPerBlock {
			return ErrStructuredKeySpace
		}
	} else if config.StageFanIn != 0 {
		return ErrUnexpectedShapeParameter
	}
	if (config.ProgramShape == ProgramShapeSelectiveReadSet || config.ProgramShape == ProgramShapeStagedFanIn) &&
		config.AccessDistribution != nil {
		return ErrStructuredDistribution
	}
	if err := validateAccessDistribution(config); err != nil {
		return err
	}
	// Int63n needs MaxComputeUnits+1 to remain a positive int64. This also
	// leaves ample room for the fixed per-transaction instruction costs.
	if config.MaxComputeUnits >= uint64(math.MaxInt64) {
		return ErrInvalidTransactionBudget
	}
	minimumUnits := config.MaxComputeUnits + 4
	if config.ProgramShape == ProgramShapeStateDependentBranch {
		minimumUnits = config.MaxComputeUnits + 7
	} else if config.ProgramShape == ProgramShapeSelectiveReadSet {
		minimumUnits = config.MaxComputeUnits + uint64(config.BranchReadCandidates) + 5
	} else if config.ProgramShape == ProgramShapeStagedFanIn {
		minimumUnits = config.MaxComputeUnits + uint64(config.StageFanIn) + 3
	}
	if config.TransactionMaxUnits < minimumUnits {
		return ErrInvalidTransactionBudget
	}
	return nil
}

func validateAccessDistribution(config Config) error {
	distribution := config.AccessDistribution
	if distribution == nil {
		return nil
	}
	if math.IsNaN(distribution.ReadWriteSameKeyProbability) ||
		distribution.ReadWriteSameKeyProbability < 0 || distribution.ReadWriteSameKeyProbability > 1 {
		return ErrInvalidReadWriteCorrelation
	}
	switch distribution.Kind {
	case AccessDistributionUniform:
		if distribution.HotKeyCount != 0 || distribution.HotAccessProbability != 0 {
			return ErrInvalidAccessDistribution
		}
	case AccessDistributionHotspot:
		if distribution.HotKeyCount <= 0 || distribution.HotKeyCount >= config.KeySpace {
			return ErrInvalidHotKeyCount
		}
		if math.IsNaN(distribution.HotAccessProbability) ||
			distribution.HotAccessProbability <= 0 || distribution.HotAccessProbability >= 1 {
			return ErrInvalidHotProbability
		}
	default:
		return ErrInvalidAccessDistribution
	}
	return nil
}

func generatorVersion(config Config) string {
	if config.ProgramShape == ProgramShapeSelectiveReadSet || config.ProgramShape == ProgramShapeStagedFanIn {
		return GeneratorVersionV3
	}
	if config.AccessDistribution != nil || config.MinComputeUnits != 0 {
		return GeneratorVersionV2
	}
	return GeneratorVersion
}

func sampleKeyIndex(rng *rand.Rand, config Config) int {
	distribution := config.AccessDistribution
	if distribution == nil || distribution.Kind == AccessDistributionUniform {
		return rng.Intn(config.KeySpace)
	}
	if rng.Float64() < distribution.HotAccessProbability {
		return rng.Intn(distribution.HotKeyCount)
	}
	return distribution.HotKeyCount + rng.Intn(config.KeySpace-distribution.HotKeyCount)
}

func sampleWriteKeyIndex(rng *rand.Rand, config Config, readKeyIndex int) (int, bool) {
	distribution := config.AccessDistribution
	if distribution != nil && (distribution.ReadWriteSameKeyProbability == 1 ||
		distribution.ReadWriteSameKeyProbability > 0 && rng.Float64() < distribution.ReadWriteSameKeyProbability) {
		return readKeyIndex, true
	}
	return sampleKeyIndex(rng, config), false
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
	explicit          *explicitProgramTruth
}

type explicitProgramTruth struct {
	path     []int
	reads    []int
	writes   []int
	branches []explicitBranchTruth
}

type explicitBranchTruth struct {
	instruction int
	taken       bool
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

func selectiveReadSetProgram(
	selectorKey []byte,
	selectorValue int64,
	candidateReadKeys [][]byte,
	writeKey []byte,
	delta int64,
	computeUnits uint64,
) ([]model.Instruction, programTruth) {
	candidateCount := len(candidateReadKeys)
	selectedCandidate := int(selectorValue) * candidateCount / 10_000
	instructions := []model.Instruction{{Op: model.OpRead, Key: selectorKey, Register: "selector"}}
	conditionalIndices := make([]int, 0, candidateCount-1)
	for candidateIndex := 0; candidateIndex < candidateCount-1; candidateIndex++ {
		conditionalIndices = append(conditionalIndices, len(instructions))
		threshold := int64(((candidateIndex+1)*10_000 + candidateCount - 1) / candidateCount)
		instructions = append(instructions, model.Instruction{
			Op: model.OpJumpIf,
			Condition: model.Condition{
				Kind:  model.ConditionLess,
				Left:  model.Register("selector"),
				Right: model.Literal(threshold),
			},
		})
	}
	defaultJumpIndex := len(instructions)
	instructions = append(instructions, model.Instruction{
		Op:        model.OpJumpIf,
		Condition: model.Condition{Kind: model.ConditionAlways},
	})

	candidateReadIndices := make([]int, candidateCount)
	candidateJumpIndices := make([]int, candidateCount-1)
	for candidateIndex, candidateKey := range candidateReadKeys {
		candidateReadIndices[candidateIndex] = len(instructions)
		instructions = append(instructions, model.Instruction{
			Op:       model.OpRead,
			Key:      candidateKey,
			Register: "value",
		})
		if candidateIndex < candidateCount-1 {
			candidateJumpIndices[candidateIndex] = len(instructions)
			instructions = append(instructions, model.Instruction{
				Op:        model.OpJumpIf,
				Condition: model.Condition{Kind: model.ConditionAlways},
			})
		}
	}
	computeIndex := len(instructions)
	instructions = append(instructions,
		model.Instruction{Op: model.OpCompute, ComputeUnits: computeUnits},
		model.Instruction{
			Op:  model.OpWrite,
			Key: writeKey,
			Expression: model.Expression{
				Base:  model.Register("value"),
				Delta: delta,
			},
		},
	)
	writeIndex := len(instructions) - 1
	for candidateIndex, conditionalIndex := range conditionalIndices {
		instructions[conditionalIndex].Target = candidateReadIndices[candidateIndex]
	}
	instructions[defaultJumpIndex].Target = candidateReadIndices[candidateCount-1]
	for _, jumpIndex := range candidateJumpIndices {
		instructions[jumpIndex].Target = computeIndex
	}

	path := []int{0}
	branchTruth := make([]explicitBranchTruth, 0, candidateCount+1)
	for candidateIndex, conditionalIndex := range conditionalIndices {
		taken := candidateIndex == selectedCandidate
		path = append(path, conditionalIndex)
		branchTruth = append(branchTruth, explicitBranchTruth{instruction: conditionalIndex, taken: taken})
		if taken {
			break
		}
	}
	if selectedCandidate == candidateCount-1 {
		path = append(path, defaultJumpIndex)
		branchTruth = append(branchTruth, explicitBranchTruth{instruction: defaultJumpIndex, taken: true})
	}
	path = append(path, candidateReadIndices[selectedCandidate])
	if selectedCandidate < candidateCount-1 {
		jumpIndex := candidateJumpIndices[selectedCandidate]
		path = append(path, jumpIndex)
		branchTruth = append(branchTruth, explicitBranchTruth{instruction: jumpIndex, taken: true})
	}
	path = append(path, computeIndex, writeIndex)

	return instructions, programTruth{explicit: &explicitProgramTruth{
		path:     path,
		reads:    []int{0, candidateReadIndices[selectedCandidate]},
		writes:   []int{writeIndex},
		branches: branchTruth,
	}}
}

func stagedFanInProgram(
	transactionIndex int,
	globalTransaction int,
	fanIn int,
	delta int64,
	computeUnits uint64,
) ([]model.Instruction, programTruth) {
	role := transactionIndex % (fanIn + 1)
	stage := transactionIndex / (fanIn + 1)
	writeKey := stateKey(globalTransaction)
	if role < fanIn {
		readKey := writeKey
		if stage > 0 {
			readKey = stateKey(globalTransaction - role - 1)
		}
		return flatProgram(readKey, writeKey, delta, computeUnits)
	}

	instructions := make([]model.Instruction, 0, fanIn+2)
	readIndices := make([]int, 0, fanIn)
	for predecessor := globalTransaction - fanIn; predecessor < globalTransaction; predecessor++ {
		readIndices = append(readIndices, len(instructions))
		register := fmt.Sprintf("fanin-%d", predecessor)
		if len(readIndices) == 1 {
			register = "value"
		}
		instructions = append(instructions, model.Instruction{
			Op:       model.OpRead,
			Key:      stateKey(predecessor),
			Register: register,
		})
	}
	instructions = append(instructions,
		model.Instruction{Op: model.OpCompute, ComputeUnits: computeUnits},
		model.Instruction{
			Op:  model.OpWrite,
			Key: writeKey,
			Expression: model.Expression{
				Base:  model.Register("value"),
				Delta: delta,
			},
		},
	)
	writeIndex := len(instructions) - 1
	path := make([]int, len(instructions))
	for index := range path {
		path[index] = index
	}
	return instructions, programTruth{explicit: &explicitProgramTruth{
		path:   path,
		reads:  readIndices,
		writes: []int{writeIndex},
	}}
}

func (truth programTruth) actualInstructionIndices(willFail bool, instructionCount int) []int {
	if truth.explicit != nil {
		indices := append([]int(nil), truth.explicit.path...)
		if willFail {
			indices = append(indices, instructionCount-2)
		} else {
			indices = append(indices, instructionCount-1)
		}
		return indices
	}
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
	if truth.explicit != nil {
		accesses := make([]workload.GroundTruthAccess, 0, len(truth.explicit.reads)+len(truth.explicit.writes))
		for _, readIndex := range truth.explicit.reads {
			accesses = append(accesses, workload.GroundTruthAccess{
				OperationID: instructions[readIndex].ID,
				Mode:        workload.AccessRead,
				Key:         cloneBytes(instructions[readIndex].Key),
			})
		}
		for _, writeIndex := range truth.explicit.writes {
			accesses = append(accesses, workload.GroundTruthAccess{
				OperationID: instructions[writeIndex].ID,
				Mode:        workload.AccessWrite,
				Key:         cloneBytes(instructions[writeIndex].Key),
			})
		}
		return accesses
	}
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
	if truth.explicit != nil {
		branches := make([]workload.BranchOutcome, 0, len(truth.explicit.branches))
		for _, branch := range truth.explicit.branches {
			branches = append(branches, workload.BranchOutcome{
				BranchID: instructions[branch.instruction].ID,
				Taken:    branch.taken,
				Target:   instructions[branch.instruction].Target,
			})
		}
		return branches
	}
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
