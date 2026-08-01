package synthetic_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math"
	"reflect"
	"testing"

	"github.com/crypto-org-chain/go-block-stm/internal/engine/serial"
	"github.com/crypto-org-chain/go-block-stm/internal/model"
	"github.com/crypto-org-chain/go-block-stm/internal/state/memkv"
	"github.com/crypto-org-chain/go-block-stm/internal/workload/synthetic"
)

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
	wantDigestBytes := sha256.Sum256(firstDescriptor)
	wantDigest := hex.EncodeToString(wantDigestBytes[:])
	if got, err := first.DescriptorDigest(); err != nil || got != wantDigest {
		t.Fatalf("unexpected descriptor digest: got %q err=%v want %q", got, err, wantDigest)
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

func TestGeneratedScenarioHasDeterministicSerialResults(t *testing.T) {
	scenario, err := synthetic.Generate(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	firstState, err := scenario.NewState()
	if err != nil {
		t.Fatal(err)
	}
	secondState, err := scenario.NewState()
	if err != nil {
		t.Fatal(err)
	}

	firstResults := executeScenario(t, scenario.Blocks, firstState)
	secondResults := executeScenario(t, scenario.Blocks, secondState)
	if !reflect.DeepEqual(firstResults, secondResults) {
		t.Fatalf("same generated scenario produced different serial results:\nfirst: %#v\nsecond: %#v", firstResults, secondResults)
	}
	if !reflect.DeepEqual(firstState.Snapshot(), secondState.Snapshot()) {
		t.Fatalf("same generated scenario produced different final states:\nfirst: %#v\nsecond: %#v", firstState.Snapshot(), secondState.Snapshot())
	}

	failed := 0
	for _, block := range firstResults {
		if block.Digest == "" || block.Digest != model.CanonicalDigest(block) {
			t.Fatalf("invalid result digest for %s: %q", block.BlockID, block.Digest)
		}
		for _, transaction := range block.Transactions {
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

func executeScenario(t *testing.T, blocks []model.Block, storage *memkv.Store) []model.BlockResult {
	t.Helper()
	engine := serial.New(nil)
	results := make([]model.BlockResult, 0, len(blocks))
	for _, block := range blocks {
		result, err := engine.ExecuteBlock(context.Background(), block, storage)
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
