package synthetic_test

import (
	"context"
	"errors"
	"math"
	"reflect"
	"testing"

	engineapi "github.com/crypto-org-chain/go-block-stm/internal/engine"
	"github.com/crypto-org-chain/go-block-stm/internal/engine/serial"
	"github.com/crypto-org-chain/go-block-stm/internal/model"
	"github.com/crypto-org-chain/go-block-stm/internal/state/memkv"
	"github.com/crypto-org-chain/go-block-stm/internal/workload"
	"github.com/crypto-org-chain/go-block-stm/internal/workload/synthetic"
)

const goldenArtifactHash = "6b71316d4076a8d0e27f078e6c52a2f9a042047e88c34ee2ea63792fafbe609d"

func TestGenerateIsByteDeterministicAndSeedSensitive(t *testing.T) {
	config := testConfig()
	first, err := synthetic.Generate(config)
	if err != nil {
		t.Fatal(err)
	}
	second, err := synthetic.Generate(config)
	if err != nil {
		t.Fatal(err)
	}
	firstDescriptor, err := first.Descriptor()
	if err != nil {
		t.Fatal(err)
	}
	secondDescriptor, err := second.Descriptor()
	if err != nil {
		t.Fatal(err)
	}
	if string(firstDescriptor) != string(secondDescriptor) {
		t.Fatalf("same seed produced different descriptors:\n%s\n%s", firstDescriptor, secondDescriptor)
	}
	if first.SchemaVersion != workload.ArtifactSchemaVersion || first.Generator.Version != synthetic.GeneratorVersion {
		t.Fatalf("unexpected artifact identity: %#v", first)
	}
	if got, err := first.DescriptorDigest(); err != nil || got != first.CanonicalHash || len(got) != 64 {
		t.Fatalf("unexpected descriptor digest: got %q err=%v field=%q", got, err, first.CanonicalHash)
	}
	if first.CanonicalHash != goldenArtifactHash {
		t.Fatalf("synthetic-v1 changed without a version bump: got %s want %s", first.CanonicalHash, goldenArtifactHash)
	}

	config.Seed++
	different, err := synthetic.Generate(config)
	if err != nil {
		t.Fatal(err)
	}
	differentDescriptor, err := different.Descriptor()
	if err != nil {
		t.Fatal(err)
	}
	if string(firstDescriptor) == string(differentDescriptor) {
		t.Fatal("different seed produced an identical descriptor")
	}
}

func TestGeneratedArtifactHasDeterministicSerialResults(t *testing.T) {
	artifact, err := synthetic.Generate(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	firstState, err := artifact.NewState()
	if err != nil {
		t.Fatal(err)
	}
	secondState, err := artifact.NewState()
	if err != nil {
		t.Fatal(err)
	}

	firstResults := executeArtifact(t, artifact.OrderedBlocks, firstState)
	secondResults := executeArtifact(t, artifact.OrderedBlocks, secondState)
	if !reflect.DeepEqual(firstResults, secondResults) {
		t.Fatalf("same generated scenario produced different serial results:\nfirst: %#v\nsecond: %#v", firstResults, secondResults)
	}
	if !reflect.DeepEqual(firstState.Snapshot(), secondState.Snapshot()) {
		t.Fatalf("same generated scenario produced different final states:\nfirst: %#v\nsecond: %#v", firstState.Snapshot(), secondState.Snapshot())
	}

	failed := 0
	groundTruthIndex := 0
	for _, block := range firstResults {
		if block.Digest == "" || block.Digest != model.CanonicalDigest(block) {
			t.Fatalf("invalid result digest for %s: %q", block.BlockID, block.Digest)
		}
		for _, transaction := range block.Transactions {
			truth := artifact.GroundTruth[groundTruthIndex]
			groundTruthIndex++
			if transaction.TransactionID != truth.TransactionID || transaction.Status != truth.ExpectedStatus {
				t.Fatalf("ground truth/result mismatch: truth=%#v result=%#v", truth, transaction)
			}
			if transaction.Status == model.TxStatusFailed {
				failed++
				if transaction.ErrorCode != "synthetic_failure" || transaction.Writes != nil {
					t.Fatalf("unexpected synthetic failure: %#v", transaction)
				}
			}
		}
	}
	if failed != 6 {
		t.Fatalf("expected 6 deterministic injected failures, got %d", failed)
	}
}

func TestGeneratedArtifactFreezesLogicalIDsAndInformationBoundary(t *testing.T) {
	artifact, err := synthetic.Generate(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	transactionCount := testConfig().BlockCount * testConfig().TransactionsPerBlock
	if len(artifact.LogicalArrivalSchedule) != transactionCount || len(artifact.GroundTruth) != transactionCount {
		t.Fatalf("artifact does not cover every transaction: arrivals=%d truth=%d transactions=%d",
			len(artifact.LogicalArrivalSchedule), len(artifact.GroundTruth), transactionCount)
	}
	if len(artifact.EngineVisibleMetadata) != 0 {
		t.Fatalf("baseline generator leaked metadata to engine: %#v", artifact.EngineVisibleMetadata)
	}

	operationIDs := make(map[string]struct{})
	for _, block := range artifact.OrderedBlocks {
		for _, transaction := range block.Transactions {
			for _, instruction := range transaction.Program.Instructions {
				if instruction.ID == "" {
					t.Fatalf("transaction %s contains an operation without a stable id", transaction.ID)
				}
				if _, exists := operationIDs[instruction.ID]; exists {
					t.Fatalf("duplicate operation id %s", instruction.ID)
				}
				operationIDs[instruction.ID] = struct{}{}
			}
		}
	}

	input, err := artifact.ExecutionInput()
	if err != nil {
		t.Fatal(err)
	}
	if len(input.Metadata) != 0 || input.ArtifactHash != artifact.CanonicalHash {
		t.Fatalf("unexpected baseline execution input: %#v", input)
	}
	if !reflect.DeepEqual(input.OrderedBlocks, artifact.OrderedBlocks) || !reflect.DeepEqual(input.LogicalArrivalSchedule, artifact.LogicalArrivalSchedule) {
		t.Fatal("execution input changed canonical workload order")
	}
	input.OrderedBlocks[0].ID = "mutated"
	if artifact.OrderedBlocks[0].ID == "mutated" {
		t.Fatal("execution input aliases the audit artifact")
	}
}

func TestGenerateRejectsInvalidConfig(t *testing.T) {
	valid := testConfig()
	tests := []struct {
		name    string
		mutate  func(*synthetic.Config)
		wantErr error
	}{
		{"initial keys", func(c *synthetic.Config) { c.InitialKeys = 0 }, synthetic.ErrInvalidInitialKeys},
		{"key space zero", func(c *synthetic.Config) { c.KeySpace = 0 }, synthetic.ErrInvalidKeySpace},
		{"key space too large", func(c *synthetic.Config) { c.KeySpace = c.InitialKeys + 1 }, synthetic.ErrInvalidKeySpace},
		{"block count", func(c *synthetic.Config) { c.BlockCount = 0 }, synthetic.ErrInvalidBlockCount},
		{"transactions", func(c *synthetic.Config) { c.TransactionsPerBlock = 0 }, synthetic.ErrInvalidTransactions},
		{"negative failure interval", func(c *synthetic.Config) { c.FailureEvery = -1 }, synthetic.ErrInvalidFailureInterval},
		{"compute range overflow", func(c *synthetic.Config) { c.MaxComputeUnits = uint64(math.MaxInt64) }, synthetic.ErrInvalidTransactionBudget},
		{"budget too small", func(c *synthetic.Config) { c.TransactionMaxUnits = c.MaxComputeUnits + 3 }, synthetic.ErrInvalidTransactionBudget},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := valid
			test.mutate(&config)
			_, err := synthetic.Generate(config)
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("expected %v, got %v", test.wantErr, err)
			}
		})
	}
}

func executeArtifact(t *testing.T, blocks []model.Block, storage *memkv.Store) []model.BlockResult {
	t.Helper()
	engine := serial.New(nil)
	results := make([]model.BlockResult, 0, len(blocks))
	for _, block := range blocks {
		result, _, err := engine.ExecuteBlock(context.Background(), block, storage, engineapi.RunConfig{Executors: 1})
		if err != nil {
			t.Fatalf("execute %s: %v", block.ID, err)
		}
		results = append(results, result)
	}
	return results
}

func testConfig() synthetic.Config {
	return synthetic.Config{
		Seed:                 42,
		InitialKeys:          8,
		KeySpace:             6,
		BlockCount:           3,
		TransactionsPerBlock: 6,
		MaxComputeUnits:      8,
		TransactionMaxUnits:  12,
		FailureEvery:         3,
	}
}
