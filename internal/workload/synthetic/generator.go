package synthetic

import (
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"sort"

	"github.com/crypto-org-chain/go-block-stm/internal/model"
	"github.com/crypto-org-chain/go-block-stm/internal/runtime/flat"
	"github.com/crypto-org-chain/go-block-stm/internal/workload"
)

const (
	GeneratorName    = "synthetic"
	GeneratorVersion = "synthetic-v4"

	TemplateRMW         = "rmw"
	TemplateReadWrite   = "read_write"
	TemplateBranch      = "state_dependent_branch"
	TemplateSelective   = "selective_read_set"
	TemplateStagedFanIn = "staged_fan_in"
	TemplateFanInFanOut = "fan_in_fan_out"
)

type Config struct {
	Seed                 int64               `json:"seed"`
	InitialKeys          int                 `json:"initial_keys"`
	KeySpace             int                 `json:"key_space"`
	BlockCount           int                 `json:"block_count"`
	TransactionsPerBlock int                 `json:"transactions_per_block"`
	FailureEvery         int                 `json:"failure_every,omitempty"`
	Mix                  []TransactionConfig `json:"mix"`
}

// TransactionConfig separates transaction semantics, key sampling and CPU cost.
// Weights are sampling weights, not exact per-block transaction counts.
// RMW reads distinct keys and updates the first UpdateKeys of those reads.
// ReadWrite samples its write set independently and uses the corresponding
// read register (cycling when there are more writes than reads).
// The remaining templates are dependency-structure diagnostics.
type TransactionConfig struct {
	Weight        float64       `json:"weight"`
	Template      string        `json:"template"`
	ReadKeys      int           `json:"read_keys,omitempty"`
	UpdateKeys    int           `json:"update_keys,omitempty"`
	CandidateKeys int           `json:"candidate_keys,omitempty"`
	FanIn         int           `json:"fan_in,omitempty"`
	Access        AccessConfig  `json:"access,omitempty"`
	Compute       ComputeConfig `json:"compute"`
}

type AccessConfig struct {
	Kind           string  `json:"kind,omitempty"`
	HotKeys        int     `json:"hot_keys,omitempty"`
	HotProbability float64 `json:"hot_probability,omitempty"`
	// Finite Zipf: P(rank) is proportional to rank^(-theta), ranks 1..KeySpace.
	// This includes the [0,1] exponents used by transactional YCSB.
	Theta float64 `json:"theta,omitempty"`
}

type ComputeConfig struct {
	MinUnits       uint64  `json:"min_units"`
	MaxUnits       uint64  `json:"max_units"`
	PrefixFraction float64 `json:"prefix_fraction"`
}

func Generate(config Config) (workload.Artifact, error) {
	if err := validateConfig(config); err != nil {
		return workload.Artifact{}, err
	}
	descriptor, err := json.Marshal(config)
	if err != nil {
		return workload.Artifact{}, err
	}
	artifact := workload.Artifact{
		SchemaVersion: workload.ArtifactSchemaVersion,
		Generator:     workload.GeneratorDescriptor{Name: GeneratorName, Version: GeneratorVersion, Seed: config.Seed, Config: descriptor},
	}
	// Cost and mixture draws never consume the key/initial-state streams.
	// Changing compute ranges or prefix placement preserves the access skeleton.
	initialRNG := rand.New(rand.NewSource(config.Seed))
	keyRNG := rand.New(rand.NewSource(config.Seed + 1))
	costRNG := rand.New(rand.NewSource(config.Seed + 2))
	mixRNG := rand.New(rand.NewSource(config.Seed + 3))
	initial := make([]int64, config.InitialKeys)
	for i := range initial {
		initial[i] = int64(initialRNG.Intn(10_000))
		artifact.InitialState = append(artifact.InitialState, model.StateEntry{Key: stateKey(i), Value: flat.EncodeInt64(initial[i])})
	}
	samplers := make([]keySampler, len(config.Mix))
	var weight float64
	for i, tx := range config.Mix {
		samplers[i] = newKeySampler(config.KeySpace, tx.Access)
		weight += tx.Weight
	}
	global := 0
	for b := 0; b < config.BlockCount; b++ {
		block := model.Block{ID: fmt.Sprintf("block-%06d", b), Height: uint64(b)}
		for t := 0; t < config.TransactionsPerBlock; t++ {
			choice := mixRNG.Float64() * weight
			selected := len(config.Mix) - 1
			for i, tx := range config.Mix {
				choice -= tx.Weight
				if choice < 0 {
					selected = i
					break
				}
			}
			tx := config.Mix[selected]
			units := tx.Compute.MinUnits + uint64(costRNG.Int63n(int64(tx.Compute.MaxUnits-tx.Compute.MinUnits+1)))
			prefix := uint64(float64(units) * tx.Compute.PrefixFraction)
			suffix := units - prefix
			delta := int64(keyRNG.Intn(11) - 5)
			sample := &samplers[selected]
			var instructions []model.Instruction
			switch tx.Template {
			case TemplateRMW, TemplateReadWrite:
				reads := sample.keys(keyRNG, tx.ReadKeys)
				var writes []int
				if tx.Template == TemplateRMW {
					writes = reads[:tx.UpdateKeys]
				} else {
					writes = sample.keys(keyRNG, tx.UpdateKeys)
				}
				instructions = linearProgram(reads, writes, delta, prefix, suffix)
			case TemplateBranch, TemplateSelective:
				selector := config.KeySpace + keyRNG.Intn(config.InitialKeys-config.KeySpace)
				var candidates []int
				var write int
				if tx.Template == TemplateBranch {
					candidates = []int{sample.key(keyRNG), sample.key(keyRNG)}
					write = sample.key(keyRNG)
				} else {
					for k := 0; k < tx.CandidateKeys; k++ {
						candidates = append(candidates, k)
					}
					write = candidates[int(initial[selector])*len(candidates)/10_000]
				}
				instructions = branchProgram(selector, candidates, write, delta, prefix, suffix)
			case TemplateStagedFanIn, TemplateFanInFanOut:
				reads := []int{global}
				if tx.Template == TemplateFanInFanOut && t >= tx.FanIn {
					reads = nil
					for k := global - t; k < global-t+tx.FanIn; k++ {
						reads = append(reads, k)
					}
				} else if tx.Template == TemplateStagedFanIn {
					role := t % (tx.FanIn + 1)
					if role == tx.FanIn {
						reads = nil
						for k := global - tx.FanIn; k < global; k++ {
							reads = append(reads, k)
						}
					} else if t > tx.FanIn {
						reads[0] = global - role - 1
					}
				}
				instructions = linearProgram(reads, []int{global}, delta, prefix, suffix)
			}
			if config.FailureEvery > 0 && (global+1)%config.FailureEvery == 0 {
				instructions = append(instructions, model.Instruction{Op: model.OpFailIf, Condition: model.Condition{Kind: model.ConditionAlways}, ErrorCode: "synthetic_failure"})
			}
			instructions = append(instructions, model.Instruction{Op: model.OpReturn, Expression: model.Expression{Base: model.Register("r0")}})
			// A conservative program budget also covers branch arms that are not
			// taken. Gas exhaustion is tested with explicit runtime programs.
			var budget uint64
			for i := range instructions {
				instructions[i].ID = fmt.Sprintf("op-%06d-%06d-%03d", b, t, i)
				budget += 1 + instructions[i].ComputeUnits
			}
			id := fmt.Sprintf("tx-%06d-%06d", b, t)
			block.Transactions = append(block.Transactions, model.Transaction{ID: id, MaxUnits: budget, Program: model.Program{Instructions: instructions}})
			artifact.LogicalArrivalSchedule = append(artifact.LogicalArrivalSchedule, workload.LogicalArrival{Sequence: uint64(global), LogicalTime: uint64(global), BlockID: block.ID, TransactionID: id})
			global++
		}
		artifact.OrderedBlocks = append(artifact.OrderedBlocks, block)
	}
	return artifact, artifact.Validate()
}

func validateConfig(c Config) error {
	bad := func(message string) error { return fmt.Errorf("invalid synthetic workload: %s", message) }
	if c.InitialKeys <= 0 || c.KeySpace <= 0 || c.KeySpace > c.InitialKeys {
		return bad("require initial_keys >= key_space > 0")
	}
	if c.BlockCount <= 0 || c.TransactionsPerBlock <= 0 || c.FailureEvery < 0 {
		return bad("positive block/transaction counts and nonnegative failure_every are required")
	}
	if len(c.Mix) == 0 {
		return bad("mix must contain a transaction template")
	}
	var totalWeight float64
	for _, tx := range c.Mix {
		if tx.Weight <= 0 || math.IsNaN(tx.Weight) || math.IsInf(tx.Weight, 0) {
			return bad("weights must be positive and finite")
		}
		totalWeight += tx.Weight
		// Float64 preserves unit counts through 2^53; bound here also keeps
		// the prefix split and generated gas budget exact and well within uint64.
		if tx.Compute.MinUnits > tx.Compute.MaxUnits || tx.Compute.MaxUnits > 1<<53 || !probability(tx.Compute.PrefixFraction) {
			return bad("invalid compute range or prefix_fraction")
		}
		support := c.KeySpace
		switch tx.Access.Kind {
		case "", "uniform":
			if tx.Access.HotKeys != 0 || tx.Access.HotProbability != 0 || tx.Access.Theta != 0 {
				return bad("uniform access does not use hotspot or Zipf parameters")
			}
		case "hotspot":
			if tx.Access.HotKeys <= 0 || tx.Access.HotKeys >= c.KeySpace || !probability(tx.Access.HotProbability) || tx.Access.Theta != 0 {
				return bad("invalid hotspot parameters")
			}
			if tx.Access.HotProbability == 1 {
				support = tx.Access.HotKeys
			}
			if tx.Access.HotProbability == 0 {
				support = c.KeySpace - tx.Access.HotKeys
			}
		case "zipf":
			if !probability(tx.Access.Theta) || tx.Access.HotKeys != 0 || tx.Access.HotProbability != 0 {
				return bad("finite Zipf requires theta in [0,1]")
			}
		default:
			return bad("unknown access distribution")
		}
		if tx.ReadKeys < 0 || tx.UpdateKeys < 0 || tx.CandidateKeys < 0 || tx.FanIn < 0 {
			return bad("negative key count")
		}
		switch tx.Template {
		case TemplateRMW, TemplateReadWrite:
			if tx.ReadKeys < 1 || tx.ReadKeys > support || tx.UpdateKeys > support {
				return bad("read/write key counts exceed the sampling support")
			}
			if tx.Template == TemplateRMW && tx.UpdateKeys > tx.ReadKeys {
				return bad("rmw updates must be a subset of reads")
			}
			if tx.CandidateKeys != 0 || tx.FanIn != 0 {
				return bad("linear templates do not use candidate_keys or fan_in")
			}
		case TemplateBranch, TemplateSelective:
			if c.InitialKeys == c.KeySpace {
				return bad("selector templates require initial_keys > key_space")
			}
			if tx.ReadKeys != 0 || tx.UpdateKeys != 0 || tx.FanIn != 0 {
				return bad("selector templates have one data read and one write")
			}
			if tx.Template == TemplateBranch && tx.CandidateKeys != 0 {
				return bad("state_dependent_branch has two sampled candidates")
			}
			if tx.Template == TemplateSelective && (tx.CandidateKeys < 2 || tx.CandidateKeys > c.KeySpace || tx.Access != (AccessConfig{})) {
				return bad("selective_read_set requires 2..key_space fixed candidates and no access distribution")
			}
		case TemplateStagedFanIn, TemplateFanInFanOut:
			if len(c.Mix) != 1 || tx.FanIn < 2 || tx.FanIn >= c.TransactionsPerBlock || c.BlockCount > c.KeySpace/c.TransactionsPerBlock {
				return bad("fan-in diagnostics require a single template, 2 <= fan_in < block size and one key per transaction")
			}
			if tx.ReadKeys != 0 || tx.UpdateKeys != 0 || tx.CandidateKeys != 0 || tx.Access != (AccessConfig{}) {
				return bad("fan-in diagnostics assign their own read/write sets")
			}
		default:
			return bad("unknown transaction template")
		}
	}
	if math.IsInf(totalWeight, 0) {
		return bad("sum of weights overflows")
	}
	return nil
}

func probability(v float64) bool { return !math.IsNaN(v) && v >= 0 && v <= 1 }

type keySampler struct {
	size   int
	access AccessConfig
	cdf    []float64
}

func newKeySampler(size int, access AccessConfig) keySampler {
	s := keySampler{size: size, access: access}
	if access.Kind == "zipf" {
		s.cdf = make([]float64, size)
		var sum float64
		for i := range s.cdf {
			sum += math.Pow(float64(i+1), -access.Theta)
			s.cdf[i] = sum
		}
	}
	return s
}

func (s *keySampler) key(rng *rand.Rand) int {
	switch s.access.Kind {
	case "hotspot":
		if rng.Float64() < s.access.HotProbability {
			return rng.Intn(s.access.HotKeys)
		}
		return s.access.HotKeys + rng.Intn(s.size-s.access.HotKeys)
	case "zipf":
		u := rng.Float64() * s.cdf[len(s.cdf)-1]
		return sort.Search(len(s.cdf), func(i int) bool { return s.cdf[i] >= u })
	default:
		return rng.Intn(s.size)
	}
}

func (s *keySampler) keys(rng *rand.Rand, count int) []int {
	keys := make([]int, 0, count)
	seen := make(map[int]bool, count)
	hotTaken := 0
	for len(keys) < count {
		var k int
		if s.access.Kind == "hotspot" {
			// Condition on keys not yet selected. Once the hot set is full,
			// draw directly from the cold tail, even for probabilities near 1.
			hot := s.access.HotKeys
			cold := s.size - hot
			h := s.access.HotProbability * float64(hot-hotTaken) / float64(hot)
			c := (1 - s.access.HotProbability) * float64(cold-(len(keys)-hotTaken)) / float64(cold)
			start, size := hot, cold
			if rng.Float64()*(h+c) < h {
				start, size = 0, hot
				hotTaken++
			}
			for {
				k = start + rng.Intn(size)
				if !seen[k] {
					break
				}
			}
		} else {
			k = s.key(rng)
		}
		if !seen[k] {
			keys = append(keys, k)
			seen[k] = true
		}
	}
	return keys
}

func linearProgram(reads, writes []int, delta int64, prefix, suffix uint64) []model.Instruction {
	p := []model.Instruction{{Op: model.OpCompute, ComputeUnits: prefix}}
	for i, key := range reads {
		p = append(p, model.Instruction{Op: model.OpRead, Key: stateKey(key), Register: fmt.Sprintf("r%d", i)})
	}
	p = append(p, model.Instruction{Op: model.OpCompute, ComputeUnits: suffix})
	for i, key := range writes {
		p = append(p, model.Instruction{Op: model.OpWrite, Key: stateKey(key), Expression: model.Expression{Base: model.Register(fmt.Sprintf("r%d", i%len(reads))), Delta: delta}})
	}
	return p
}

// Selector keys are in the initialized, read-only tail [KeySpace, InitialKeys).
// COMPUTE models CPU cost; JUMP consumes the selector value, not a computed
// address. A common suffix/write keeps conservative reads and exact writes
// separate for the existing selective-read diagnostic.
func branchProgram(selector int, candidates []int, write int, delta int64, prefix, suffix uint64) []model.Instruction {
	p := []model.Instruction{
		{Op: model.OpRead, Key: stateKey(selector), Register: "selector"},
		{Op: model.OpCompute, ComputeUnits: prefix},
	}
	for i := 0; i < len(candidates)-1; i++ {
		threshold := int64(((i+1)*10_000 + len(candidates) - 1) / len(candidates))
		p = append(p, model.Instruction{Op: model.OpJumpIf, Condition: model.Condition{Kind: model.ConditionLess, Left: model.Register("selector"), Right: model.Literal(threshold)}})
	}
	fallback := len(p)
	p = append(p, model.Instruction{Op: model.OpJumpIf, Condition: model.Condition{Kind: model.ConditionAlways}})
	var exits []int
	for i, key := range candidates {
		if i < len(candidates)-1 {
			p[2+i].Target = len(p)
		} else {
			p[fallback].Target = len(p)
		}
		p = append(p, model.Instruction{Op: model.OpRead, Key: stateKey(key), Register: "r0"})
		exits = append(exits, len(p))
		p = append(p, model.Instruction{Op: model.OpJumpIf, Condition: model.Condition{Kind: model.ConditionAlways}})
	}
	for _, exit := range exits {
		p[exit].Target = len(p)
	}
	p = append(p,
		model.Instruction{Op: model.OpCompute, ComputeUnits: suffix},
		model.Instruction{Op: model.OpWrite, Key: stateKey(write), Expression: model.Expression{Base: model.Register("r0"), Delta: delta}},
	)
	return p
}

func stateKey(i int) []byte { return []byte(fmt.Sprintf("key-%08d", i)) }
