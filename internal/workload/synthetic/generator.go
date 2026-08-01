package synthetic

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/rand"

	"github.com/crypto-org-chain/go-block-stm/internal/model"
	"github.com/crypto-org-chain/go-block-stm/internal/runtime/flat"
	"github.com/crypto-org-chain/go-block-stm/internal/state/memkv"
)

const descriptorSchema = "synthetic-workload-v1"

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

type Scenario struct {
	Schema       string             `json:"schema"`
	Config       Config             `json:"config"`
	InitialState []model.StateEntry `json:"initial_state"`
	Blocks       []model.Block      `json:"blocks"`
}

func Generate(config Config) (Scenario, error) {
	if err := validateConfig(config); err != nil {
		return Scenario{}, err
	}

	rng := rand.New(rand.NewSource(config.Seed))
	scenario := Scenario{
		Schema:       descriptorSchema,
		Config:       config,
		InitialState: make([]model.StateEntry, 0, config.InitialKeys),
		Blocks:       make([]model.Block, 0, config.BlockCount),
	}

	for i := 0; i < config.InitialKeys; i++ {
		scenario.InitialState = append(scenario.InitialState, model.StateEntry{
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
			globalTransaction++
			if config.FailureEvery > 0 && globalTransaction%config.FailureEvery == 0 {
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

			block.Transactions = append(block.Transactions, model.Transaction{
				ID:       fmt.Sprintf("tx-%06d-%06d", blockIndex, transactionIndex),
				MaxUnits: config.TransactionMaxUnits,
				Program:  model.Program{Instructions: instructions},
			})
		}
		scenario.Blocks = append(scenario.Blocks, block)
	}

	return scenario, nil
}

func (s Scenario) Descriptor() ([]byte, error) {
	return json.Marshal(s)
}

func (s Scenario) DescriptorDigest() (string, error) {
	descriptor, err := s.Descriptor()
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(descriptor)
	return hex.EncodeToString(digest[:]), nil
}

func (s Scenario) NewState() (*memkv.Store, error) {
	return memkv.FromEntries(s.InitialState)
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
