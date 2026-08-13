package synthetic_test

import (
	"context"
	"errors"
	"fmt"
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

func TestHotspotAccessDistributionIsDeterministicAndKeepsAColdTail(t *testing.T) {
	config := testConfig()
	config.InitialKeys = 1_000
	config.KeySpace = 1_000
	config.BlockCount = 1
	config.TransactionsPerBlock = 5_000
	config.FailureEvery = 0
	config.AccessDistribution = &synthetic.AccessDistributionConfig{
		Kind:                 synthetic.AccessDistributionHotspot,
		HotKeyCount:          4,
		HotAccessProbability: 0.9,
	}

	first, err := synthetic.Generate(config)
	if err != nil {
		t.Fatal(err)
	}
	second, err := synthetic.Generate(config)
	if err != nil {
		t.Fatal(err)
	}
	if first.CanonicalHash != second.CanonicalHash {
		t.Fatalf("hotspot workload is not deterministic: %s != %s", first.CanonicalHash, second.CanonicalHash)
	}
	if first.Generator.Version != synthetic.GeneratorVersionV2 {
		t.Fatalf("got generator version %q", first.Generator.Version)
	}

	hotKeys := map[string]struct{}{
		"key-00000000": {},
		"key-00000001": {},
		"key-00000002": {},
		"key-00000003": {},
	}
	hot, cold := 0, 0
	for _, truth := range first.GroundTruth {
		for _, access := range truth.Accesses {
			if _, ok := hotKeys[string(access.Key)]; ok {
				hot++
			} else {
				cold++
			}
		}
	}
	ratio := float64(hot) / float64(hot+cold)
	if ratio < 0.88 || ratio > 0.92 {
		t.Fatalf("hot access ratio %f is inconsistent with configured probability", ratio)
	}
	if cold == 0 {
		t.Fatal("hotspot workload did not retain cold-tail accesses")
	}
}

func TestComputeRangeCanBeFixed(t *testing.T) {
	config := testConfig()
	config.MinComputeUnits = config.MaxComputeUnits
	artifact, err := synthetic.Generate(config)
	if err != nil {
		t.Fatal(err)
	}
	if artifact.Generator.Version != synthetic.GeneratorVersionV2 {
		t.Fatalf("got generator version %q", artifact.Generator.Version)
	}
	for _, block := range artifact.OrderedBlocks {
		for _, transaction := range block.Transactions {
			for _, instruction := range transaction.Program.Instructions {
				if instruction.Op == model.OpCompute && instruction.ComputeUnits != config.MaxComputeUnits {
					t.Fatalf("got compute units %d, want %d", instruction.ComputeUnits, config.MaxComputeUnits)
				}
			}
		}
	}
}

func TestAccessDistributionCanCorrelateReadAndWriteKeys(t *testing.T) {
	config := testConfig()
	config.InitialKeys = 1_000
	config.KeySpace = 1_000
	config.AccessDistribution = &synthetic.AccessDistributionConfig{
		Kind:                        synthetic.AccessDistributionHotspot,
		HotKeyCount:                 8,
		HotAccessProbability:        0.9,
		ReadWriteSameKeyProbability: 1,
	}
	artifact, err := synthetic.Generate(config)
	if err != nil {
		t.Fatal(err)
	}
	for _, truth := range artifact.GroundTruth {
		if len(truth.Accesses) != 2 || string(truth.Accesses[0].Key) != string(truth.Accesses[1].Key) {
			t.Fatalf("transaction %s does not read and write the same key: %#v", truth.TransactionID, truth.Accesses)
		}
	}
}

func TestStateDependentCorrelationUsesTheExecutedBranchRead(t *testing.T) {
	config := testConfig()
	config.InitialKeys = 16
	config.KeySpace = 8
	config.ProgramShape = synthetic.ProgramShapeStateDependentBranch
	config.TransactionMaxUnits = config.MaxComputeUnits + 7
	config.AccessDistribution = &synthetic.AccessDistributionConfig{
		Kind:                        synthetic.AccessDistributionUniform,
		ReadWriteSameKeyProbability: 1,
	}
	artifact, err := synthetic.Generate(config)
	if err != nil {
		t.Fatal(err)
	}
	for _, truth := range artifact.GroundTruth {
		if len(truth.Accesses) != 3 || string(truth.Accesses[1].Key) != string(truth.Accesses[2].Key) {
			t.Fatalf("transaction %s write does not match its executed branch read: %#v", truth.TransactionID, truth.Accesses)
		}
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

func TestStateDependentBranchShapeRecordsActualPathWithoutLeakingIt(t *testing.T) {
	config := synthetic.Config{
		Seed:                 73,
		InitialKeys:          16,
		KeySpace:             8,
		BlockCount:           1,
		TransactionsPerBlock: 32,
		MaxComputeUnits:      1,
		TransactionMaxUnits:  8,
		ProgramShape:         synthetic.ProgramShapeStateDependentBranch,
	}
	artifact, err := synthetic.Generate(config)
	if err != nil {
		t.Fatal(err)
	}
	state, err := artifact.NewState()
	if err != nil {
		t.Fatal(err)
	}
	results := executeArtifact(t, artifact.OrderedBlocks, state)
	if len(results) != 1 || len(results[0].Transactions) != config.TransactionsPerBlock {
		t.Fatalf("unexpected result shape: %#v", results)
	}

	taken, untaken := 0, 0
	for index, transaction := range results[0].Transactions {
		truth := artifact.GroundTruth[index]
		if len(transaction.Reads) != 2 || len(truth.Accesses) != 3 {
			t.Fatalf("transaction %d did not execute selector + one branch read + write: result=%#v truth=%#v", index, transaction, truth)
		}
		readInstructions := 0
		for _, instruction := range artifact.OrderedBlocks[0].Transactions[index].Program.Instructions {
			if instruction.Op == model.OpRead {
				readInstructions++
			}
		}
		if readInstructions != 3 {
			t.Fatalf("transaction %d does not expose both syntactic branch reads", index)
		}
		if len(truth.Branches) == 0 {
			t.Fatalf("transaction %d omits branch ground truth", index)
		}
		if truth.Branches[0].Taken {
			taken++
		} else {
			untaken++
		}
	}
	if taken == 0 || untaken == 0 {
		t.Fatalf("seed did not exercise both state-dependent paths: taken=%d untaken=%d", taken, untaken)
	}
	if len(artifact.EngineVisibleMetadata) != 0 {
		t.Fatalf("branch ground truth leaked through metadata: %#v", artifact.EngineVisibleMetadata)
	}
}

func TestSelectiveReadSetExposesCandidatesButExecutesOneRead(t *testing.T) {
	config := synthetic.Config{
		Seed:                 97,
		InitialKeys:          80,
		KeySpace:             16,
		BlockCount:           1,
		TransactionsPerBlock: 64,
		MaxComputeUnits:      1,
		TransactionMaxUnits:  14,
		ProgramShape:         synthetic.ProgramShapeSelectiveReadSet,
		BranchReadCandidates: 8,
	}
	artifact, err := synthetic.Generate(config)
	if err != nil {
		t.Fatal(err)
	}
	if artifact.Generator.Version != synthetic.GeneratorVersionV3 {
		t.Fatalf("got generator version %q", artifact.Generator.Version)
	}
	state, err := artifact.NewState()
	if err != nil {
		t.Fatal(err)
	}
	results := executeArtifact(t, artifact.OrderedBlocks, state)
	selectedKeys := make(map[string]struct{})
	for index, transaction := range results[0].Transactions {
		truth := artifact.GroundTruth[index]
		if len(transaction.Reads) != 2 || len(truth.Accesses) != 3 {
			t.Fatalf("transaction %d did not execute selector + one candidate read + write: result=%#v truth=%#v", index, transaction, truth)
		}
		readInstructions := 0
		for _, instruction := range artifact.OrderedBlocks[0].Transactions[index].Program.Instructions {
			if instruction.Op == model.OpRead {
				readInstructions++
			}
		}
		if readInstructions != config.BranchReadCandidates+1 {
			t.Fatalf("transaction %d exposes %d reads, want %d", index, readInstructions, config.BranchReadCandidates+1)
		}
		if string(truth.Accesses[1].Key) != string(truth.Accesses[2].Key) {
			t.Fatalf("transaction %d write does not match selected read: %#v", index, truth.Accesses)
		}
		if string(transaction.Reads[1].Key) != string(truth.Accesses[1].Key) {
			t.Fatalf("transaction %d executed read %q, ground truth says %q", index, transaction.Reads[1].Key, truth.Accesses[1].Key)
		}
		selectedKeys[string(truth.Accesses[1].Key)] = struct{}{}
	}
	if len(selectedKeys) < 2 {
		t.Fatalf("seed selected only %d candidate keys", len(selectedKeys))
	}
}

func TestStagedFanInBuildsParallelProducerWaves(t *testing.T) {
	const fanIn = 5
	config := synthetic.Config{
		Seed:                 101,
		InitialKeys:          18,
		KeySpace:             18,
		BlockCount:           1,
		TransactionsPerBlock: 18,
		MaxComputeUnits:      1,
		TransactionMaxUnits:  9,
		ProgramShape:         synthetic.ProgramShapeStagedFanIn,
		StageFanIn:           fanIn,
	}
	artifact, err := synthetic.Generate(config)
	if err != nil {
		t.Fatal(err)
	}
	if artifact.Generator.Version != synthetic.GeneratorVersionV3 {
		t.Fatalf("got generator version %q", artifact.Generator.Version)
	}
	state, err := artifact.NewState()
	if err != nil {
		t.Fatal(err)
	}
	results := executeArtifact(t, artifact.OrderedBlocks, state)
	for index, transaction := range results[0].Transactions {
		truth := artifact.GroundTruth[index]
		role := index % (fanIn + 1)
		stage := index / (fanIn + 1)
		if role == fanIn {
			if len(transaction.Reads) != fanIn || len(truth.Accesses) != fanIn+1 {
				t.Fatalf("consumer %d does not read its full producer wave: result=%#v truth=%#v", index, transaction, truth)
			}
			for predecessor := 0; predecessor < fanIn; predecessor++ {
				want := fmt.Sprintf("key-%08d", index-fanIn+predecessor)
				if string(truth.Accesses[predecessor].Key) != want {
					t.Fatalf("consumer %d read %q, want %q", index, truth.Accesses[predecessor].Key, want)
				}
			}
			continue
		}
		if len(transaction.Reads) != 1 || len(truth.Accesses) != 2 {
			t.Fatalf("producer %d has unexpected accesses: result=%#v truth=%#v", index, transaction, truth)
		}
		wantRead := index
		if stage > 0 {
			wantRead = index - role - 1
		}
		if got := string(truth.Accesses[0].Key); got != fmt.Sprintf("key-%08d", wantRead) {
			t.Fatalf("producer %d read %q, want prior barrier key %d", index, got, wantRead)
		}
	}
}

func TestFanInFanOutSharesOneExactProducerBarrier(t *testing.T) {
	const fanIn = 5
	config := synthetic.Config{
		Seed:                 103,
		InitialKeys:          18,
		KeySpace:             18,
		BlockCount:           1,
		TransactionsPerBlock: 18,
		MaxComputeUnits:      1,
		TransactionMaxUnits:  9,
		ProgramShape:         synthetic.ProgramShapeFanInFanOut,
		FanIn:                fanIn,
	}
	artifact, err := synthetic.Generate(config)
	if err != nil {
		t.Fatal(err)
	}
	state, err := artifact.NewState()
	if err != nil {
		t.Fatal(err)
	}
	results := executeArtifact(t, artifact.OrderedBlocks, state)
	for index, transaction := range results[0].Transactions {
		truth := artifact.GroundTruth[index]
		if index < fanIn {
			if len(transaction.Reads) != 1 || len(truth.Accesses) != 2 {
				t.Fatalf("producer %d has unexpected accesses: result=%#v truth=%#v", index, transaction, truth)
			}
			want := fmt.Sprintf("key-%08d", index)
			if string(truth.Accesses[0].Key) != want || string(truth.Accesses[1].Key) != want {
				t.Fatalf("producer %d does not own key %q: %#v", index, want, truth.Accesses)
			}
			continue
		}
		if len(transaction.Reads) != fanIn || len(truth.Accesses) != fanIn+1 {
			t.Fatalf("consumer %d does not read the producer prefix: result=%#v truth=%#v", index, transaction, truth)
		}
		for predecessor := 0; predecessor < fanIn; predecessor++ {
			want := fmt.Sprintf("key-%08d", predecessor)
			if string(transaction.Reads[predecessor].Key) != want || string(truth.Accesses[predecessor].Key) != want {
				t.Fatalf("consumer %d read %q, want %q", index, truth.Accesses[predecessor].Key, want)
			}
		}
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
		{"compute range", func(c *synthetic.Config) { c.MinComputeUnits = c.MaxComputeUnits + 1 }, synthetic.ErrInvalidComputeRange},
		{"unknown program shape", func(c *synthetic.Config) { c.ProgramShape = "unknown" }, synthetic.ErrInvalidProgramShape},
		{"unknown access distribution", func(c *synthetic.Config) {
			c.AccessDistribution = &synthetic.AccessDistributionConfig{Kind: "unknown"}
		}, synthetic.ErrInvalidAccessDistribution},
		{"uniform with hotspot fields", func(c *synthetic.Config) {
			c.AccessDistribution = &synthetic.AccessDistributionConfig{
				Kind:        synthetic.AccessDistributionUniform,
				HotKeyCount: 1,
			}
		}, synthetic.ErrInvalidAccessDistribution},
		{"hot key count zero", func(c *synthetic.Config) {
			c.AccessDistribution = &synthetic.AccessDistributionConfig{
				Kind:                 synthetic.AccessDistributionHotspot,
				HotAccessProbability: 0.9,
			}
		}, synthetic.ErrInvalidHotKeyCount},
		{"hot key count covers key space", func(c *synthetic.Config) {
			c.AccessDistribution = &synthetic.AccessDistributionConfig{
				Kind:                 synthetic.AccessDistributionHotspot,
				HotKeyCount:          c.KeySpace,
				HotAccessProbability: 0.9,
			}
		}, synthetic.ErrInvalidHotKeyCount},
		{"hot probability zero", func(c *synthetic.Config) {
			c.AccessDistribution = &synthetic.AccessDistributionConfig{
				Kind:        synthetic.AccessDistributionHotspot,
				HotKeyCount: 1,
			}
		}, synthetic.ErrInvalidHotProbability},
		{"hot probability one", func(c *synthetic.Config) {
			c.AccessDistribution = &synthetic.AccessDistributionConfig{
				Kind:                 synthetic.AccessDistributionHotspot,
				HotKeyCount:          1,
				HotAccessProbability: 1,
			}
		}, synthetic.ErrInvalidHotProbability},
		{"hot probability NaN", func(c *synthetic.Config) {
			c.AccessDistribution = &synthetic.AccessDistributionConfig{
				Kind:                 synthetic.AccessDistributionHotspot,
				HotKeyCount:          1,
				HotAccessProbability: math.NaN(),
			}
		}, synthetic.ErrInvalidHotProbability},
		{"read/write correlation negative", func(c *synthetic.Config) {
			c.AccessDistribution = &synthetic.AccessDistributionConfig{
				Kind:                        synthetic.AccessDistributionUniform,
				ReadWriteSameKeyProbability: -0.1,
			}
		}, synthetic.ErrInvalidReadWriteCorrelation},
		{"read/write correlation above one", func(c *synthetic.Config) {
			c.AccessDistribution = &synthetic.AccessDistributionConfig{
				Kind:                        synthetic.AccessDistributionUniform,
				ReadWriteSameKeyProbability: 1.1,
			}
		}, synthetic.ErrInvalidReadWriteCorrelation},
		{"read/write correlation NaN", func(c *synthetic.Config) {
			c.AccessDistribution = &synthetic.AccessDistributionConfig{
				Kind:                        synthetic.AccessDistributionUniform,
				ReadWriteSameKeyProbability: math.NaN(),
			}
		}, synthetic.ErrInvalidReadWriteCorrelation},
		{"branch selector key space", func(c *synthetic.Config) {
			c.ProgramShape = synthetic.ProgramShapeStateDependentBranch
			c.KeySpace = c.InitialKeys
		}, synthetic.ErrBranchKeySpace},
		{"branch budget too small", func(c *synthetic.Config) {
			c.ProgramShape = synthetic.ProgramShapeStateDependentBranch
			c.TransactionMaxUnits = c.MaxComputeUnits + 6
		}, synthetic.ErrInvalidTransactionBudget},
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
