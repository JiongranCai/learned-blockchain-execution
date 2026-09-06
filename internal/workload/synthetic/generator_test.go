package synthetic_test

import (
	"context"
	"math"
	"reflect"
	"testing"

	engineapi "github.com/crypto-org-chain/go-block-stm/internal/engine"
	"github.com/crypto-org-chain/go-block-stm/internal/engine/serial"
	"github.com/crypto-org-chain/go-block-stm/internal/model"
	"github.com/crypto-org-chain/go-block-stm/internal/workload"
	"github.com/crypto-org-chain/go-block-stm/internal/workload/synthetic"
)

func config() synthetic.Config {
	return synthetic.Config{Seed: 42, InitialKeys: 64, KeySpace: 32, BlockCount: 2, TransactionsPerBlock: 32,
		Mix: []synthetic.TransactionConfig{{Weight: 1, Template: synthetic.TemplateRMW, ReadKeys: 4, UpdateKeys: 2,
			Compute: synthetic.ComputeConfig{MinUnits: 128, MaxUnits: 128}}}}
}

func generate(t *testing.T, c synthetic.Config) workload.Artifact {
	t.Helper()
	a, err := synthetic.Generate(c)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func execute(t *testing.T, a workload.Artifact) []model.BlockResult {
	t.Helper()
	s, err := a.NewState()
	if err != nil {
		t.Fatal(err)
	}
	var results []model.BlockResult
	for _, block := range a.OrderedBlocks {
		r, _, err := serial.New(nil).ExecuteBlock(context.Background(), block, s, engineapi.RunConfig{})
		if err != nil {
			t.Fatal(err)
		}
		results = append(results, r)
	}
	return results
}

func TestDeterminismAndSeedSensitivity(t *testing.T) {
	c := config()
	a, b := generate(t, c), generate(t, c)
	left, err := a.Descriptor()
	if err != nil {
		t.Fatal(err)
	}
	right, err := b.Descriptor()
	if err != nil {
		t.Fatal(err)
	}
	if string(left) != string(right) || !reflect.DeepEqual(execute(t, a), execute(t, b)) {
		t.Fatal("same seed changed workload or execution")
	}
	c.Seed++
	if reflect.DeepEqual(a, generate(t, c)) {
		t.Fatal("different seed produced the same workload")
	}
	if a.Generator.Version != synthetic.GeneratorVersion {
		t.Fatal(a.Generator.Version)
	}
}

func TestCostChangesPreserveAccessesMixAndOrder(t *testing.T) {
	c := config()
	c.Mix = append(c.Mix, synthetic.TransactionConfig{Weight: 1, Template: synthetic.TemplateSelective, CandidateKeys: 4,
		Compute: synthetic.ComputeConfig{MinUnits: 64, MaxUnits: 128}})
	base := generate(t, c)
	baseResults := execute(t, base)
	for _, fraction := range []float64{0, 0.5, 0.9, 1} {
		for i := range c.Mix {
			c.Mix[i].Compute.PrefixFraction = fraction
		}
		a := generate(t, c)
		assertSameSkeleton(t, base, a)
		results := execute(t, a)
		for b := range results {
			if !reflect.DeepEqual(results[b].FinalState, baseResults[b].FinalState) {
				t.Fatal("prefix placement changed final state")
			}
			for i, got := range results[b].Transactions {
				want := baseResults[b].Transactions[i]
				if got.UnitsUsed != want.UnitsUsed || !reflect.DeepEqual(got.Reads, want.Reads) || !reflect.DeepEqual(got.Writes, want.Writes) {
					t.Fatal("prefix placement changed work or accesses")
				}
			}
			for i, tx := range a.OrderedBlocks[b].Transactions {
				var got, want uint64
				for _, op := range tx.Program.Instructions {
					got += op.ComputeUnits
				}
				for _, op := range base.OrderedBlocks[b].Transactions[i].Program.Instructions {
					want += op.ComputeUnits
				}
				if got != want {
					t.Fatal("prefix split changed total CPU work")
				}
			}
		}
	}
	for i := range c.Mix {
		c.Mix[i].Compute.MinUnits, c.Mix[i].Compute.MaxUnits = 0, 513
	}
	assertSameSkeleton(t, base, generate(t, c))
}

func assertSameSkeleton(t *testing.T, a, b workload.Artifact) {
	t.Helper()
	if !reflect.DeepEqual(a.InitialState, b.InitialState) || !reflect.DeepEqual(a.LogicalArrivalSchedule, b.LogicalArrivalSchedule) {
		t.Fatal("cost changed state or order")
	}
	for i, block := range a.OrderedBlocks {
		for j, tx := range block.Transactions {
			other := b.OrderedBlocks[i].Transactions[j]
			if tx.ID != other.ID || len(tx.Program.Instructions) != len(other.Program.Instructions) {
				t.Fatal("cost changed transaction layout")
			}
			for k, op := range tx.Program.Instructions {
				want := other.Program.Instructions[k]
				op.ComputeUnits, want.ComputeUnits = 0, 0
				if !reflect.DeepEqual(op, want) {
					t.Fatalf("cost changed operation %s", op.ID)
				}
			}
		}
	}
}

func TestRMWAndReadOnlySemantics(t *testing.T) {
	for _, updates := range []int{0, 2, 4} {
		c := config()
		c.Mix[0].UpdateKeys = updates
		for _, block := range execute(t, generate(t, c)) {
			for _, tx := range block.Transactions {
				if tx.Status != model.TxStatusSuccess || len(tx.Reads) != 4 || len(tx.Writes) != updates {
					t.Fatalf("bad read/write footprint: %#v", tx)
				}
				reads := map[string]bool{}
				for _, r := range tx.Reads {
					reads[string(r.Key)] = true
				}
				if len(reads) != 4 {
					t.Fatal("duplicate read key")
				}
				for _, w := range tx.Writes {
					if !reads[string(w.Key)] {
						t.Fatal("RMW wrote an unread key")
					}
				}
			}
		}
	}
}

func TestSelectorReadPrecedesPrefixAndControlsExecutedRead(t *testing.T) {
	c := config()
	c.Mix = []synthetic.TransactionConfig{{Weight: 1, Template: synthetic.TemplateSelective, CandidateKeys: 4,
		Compute: synthetic.ComputeConfig{MinUnits: 20, MaxUnits: 20, PrefixFraction: 0.5}}}
	a := generate(t, c)
	for b, result := range execute(t, a) {
		for i, tx := range result.Transactions {
			ops := a.OrderedBlocks[b].Transactions[i].Program.Instructions
			if ops[0].Op != model.OpRead || ops[0].Register != "selector" || ops[1].Op != model.OpCompute || ops[1].ComputeUnits != 10 {
				t.Fatal("selector/prefix order is incorrect")
			}
			if tx.Status != model.TxStatusSuccess || len(tx.Reads) != 2 || len(tx.Writes) != 1 {
				t.Fatalf("wrong branch execution: %#v", tx)
			}
			if string(tx.Reads[1].Key) != string(tx.Writes[0].Key) {
				t.Fatal("selective read and concrete write disagree")
			}
		}
	}
	// A concrete program must react to state at execution, not to generator
	// ground truth. Flip the first selector across the candidate thresholds.
	first := a.OrderedBlocks[0].Transactions[0]
	for i := range a.InitialState {
		if string(a.InitialState[i].Key) == string(first.Program.Instructions[0].Key) {
			a.InitialState[i].Value = []byte{0, 0, 0, 0, 0, 0, 0, 0}
		}
	}
	zero := execute(t, a)[0].Transactions[0].Reads[1].Key
	for i := range a.InitialState {
		if string(a.InitialState[i].Key) == string(first.Program.Instructions[0].Key) {
			a.InitialState[i].Value = []byte{0, 0, 0, 0, 0, 0, 0x27, 0x0f}
		}
	}
	last := execute(t, a)[0].Transactions[0].Reads[1].Key
	if string(zero) == string(last) {
		t.Fatal("branch ignored the runtime selector value")
	}
}

func TestAccessDistributions(t *testing.T) {
	for _, access := range []synthetic.AccessConfig{
		{Kind: "uniform"}, {Kind: "hotspot", HotKeys: 4, HotProbability: 0.8}, {Kind: "zipf", Theta: 0.9},
	} {
		c := config()
		c.BlockCount, c.TransactionsPerBlock = 1, 5000
		c.Mix[0].ReadKeys, c.Mix[0].UpdateKeys = 1, 1
		c.Mix[0].Access = access
		a := generate(t, c)
		counts := make(map[string]int)
		for _, tx := range a.OrderedBlocks[0].Transactions {
			counts[string(tx.Program.Instructions[1].Key)]++
		}
		head := counts["key-00000000"] + counts["key-00000001"] + counts["key-00000002"] + counts["key-00000003"]
		switch access.Kind {
		case "uniform":
			if head < 500 || head > 750 {
				t.Fatalf("uniform head count: %d", head)
			}
		case "hotspot":
			if head < 3800 || head > 4200 {
				t.Fatalf("hotspot head count: %d", head)
			}
		case "zipf":
			if counts["key-00000000"] < 8*counts["key-00000031"] {
				t.Fatal("Zipf does not produce a heavy head")
			}
		}
		if len(counts) != c.KeySpace {
			t.Fatal("distribution lost its tail")
		}
	}
	// Exhausting the hot set must still allow distinct cold reads, without
	// waiting for an extremely unlikely unconditional cold draw.
	for _, p := range []float64{0, math.Nextafter(1, 0), 1} {
		c := config()
		c.BlockCount, c.TransactionsPerBlock = 1, 4
		c.Mix[0].Access = synthetic.AccessConfig{Kind: "hotspot", HotKeys: 4, HotProbability: p}
		if p != 1 {
			c.Mix[0].Access.HotKeys = 1
		}
		for _, tx := range execute(t, generate(t, c))[0].Transactions {
			keys := map[string]bool{}
			for _, r := range tx.Reads {
				keys[string(r.Key)] = true
			}
			if len(keys) != 4 {
				t.Fatal("hotspot sampling did not fill the distinct read set")
			}
			if p == 0 && keys["key-00000000"] {
				t.Fatal("zero-probability hot key was selected")
			}
			if p == 1 && !keys["key-00000003"] {
				t.Fatal("full-hot sampling missed a hot key")
			}
		}
	}
}

func TestStructuredTemplatesAndFailureRollback(t *testing.T) {
	for _, template := range []string{synthetic.TemplateReadWrite, synthetic.TemplateBranch, synthetic.TemplateStagedFanIn, synthetic.TemplateFanInFanOut} {
		c := config()
		c.InitialKeys, c.KeySpace = 80, 64
		c.FailureEvery = 7
		p := synthetic.TransactionConfig{Weight: 1, Template: template, Compute: synthetic.ComputeConfig{MaxUnits: 64, PrefixFraction: 0.5}}
		if template == synthetic.TemplateReadWrite {
			p.ReadKeys, p.UpdateKeys = 2, 3
		}
		if template == synthetic.TemplateStagedFanIn || template == synthetic.TemplateFanInFanOut {
			p.FanIn = 4
		}
		c.Mix = []synthetic.TransactionConfig{p}
		index := 0
		results := execute(t, generate(t, c))
		if template == synthetic.TemplateStagedFanIn || template == synthetic.TemplateFanInFanOut {
			keys := func(tx model.TxResult) []string {
				var result []string
				for _, r := range tx.Reads {
					result = append(result, string(r.Key))
				}
				return result
			}
			producers := []string{"key-00000000", "key-00000001", "key-00000002", "key-00000003"}
			if !reflect.DeepEqual(keys(results[0].Transactions[4]), producers) {
				t.Fatal("join lost producer dependencies")
			}
			want := producers
			if template == synthetic.TemplateStagedFanIn {
				want = []string{"key-00000004"}
			}
			if !reflect.DeepEqual(keys(results[0].Transactions[5]), want) {
				t.Fatal("fan-out/stage boundary changed")
			}
			if !reflect.DeepEqual(keys(results[1].Transactions[4]), []string{"key-00000032", "key-00000033", "key-00000034", "key-00000035"}) {
				t.Fatal("dependency graph crossed block boundaries")
			}
		}
		for _, block := range results {
			for _, tx := range block.Transactions {
				index++
				want := model.TxStatusSuccess
				if index%7 == 0 {
					want = model.TxStatusFailed
					if len(tx.Writes) != 0 {
						t.Fatal("failed transaction published writes")
					}
				}
				if tx.Status != want {
					t.Fatalf("%s: tx %d got %s, want %s", template, index, tx.Status, want)
				}
			}
		}
	}
}

func TestInvalidParameters(t *testing.T) {
	cases := []func(*synthetic.Config){
		func(c *synthetic.Config) { c.KeySpace = 0 },
		func(c *synthetic.Config) { c.BlockCount = 0 },
		func(c *synthetic.Config) { c.Mix = nil },
		func(c *synthetic.Config) { c.Mix[0].Weight = math.NaN() },
		func(c *synthetic.Config) { c.Mix[0].Weight = 0 },
		func(c *synthetic.Config) { c.Mix[0].Template = "unknown" },
		func(c *synthetic.Config) { c.Mix[0].Compute.MinUnits = 129 },
		func(c *synthetic.Config) { c.Mix[0].Compute.PrefixFraction = -0.1 },
		func(c *synthetic.Config) { c.Mix[0].UpdateKeys = 5 },
		func(c *synthetic.Config) { c.Mix[0].Access = synthetic.AccessConfig{Kind: "zipf", Theta: 1.1} },
		func(c *synthetic.Config) {
			c.Mix[0].Access = synthetic.AccessConfig{Kind: "hotspot", HotKeys: 1, HotProbability: 1}
		},
	}
	for i, mutate := range cases {
		c := config()
		mutate(&c)
		if _, err := synthetic.Generate(c); err == nil {
			t.Fatalf("invalid case %d was accepted", i)
		}
	}
}
