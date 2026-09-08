package workload

import (
	"fmt"
	"math"
	"math/rand"
	"sort"
)

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

// Support validates the distribution and returns the number of reachable IDs.
func (a AccessConfig) Support(size int) (int, error) {
	support := size
	switch a.Kind {
	case "", "uniform":
		if a.HotKeys != 0 || a.HotProbability != 0 || a.Theta != 0 {
			return 0, fmt.Errorf("uniform access does not use hotspot or Zipf parameters")
		}
	case "hotspot":
		if a.HotKeys <= 0 || a.HotKeys >= size || !probability(a.HotProbability) || a.Theta != 0 {
			return 0, fmt.Errorf("invalid hotspot parameters")
		}
		if a.HotProbability == 1 {
			support = a.HotKeys
		}
		if a.HotProbability == 0 {
			support = size - a.HotKeys
		}
	case "zipf":
		if !probability(a.Theta) || a.HotKeys != 0 || a.HotProbability != 0 {
			return 0, fmt.Errorf("finite Zipf requires theta in [0,1]")
		}
	default:
		return 0, fmt.Errorf("unknown access distribution %q", a.Kind)
	}
	return support, nil
}

func (c ComputeConfig) Validate() error {
	if c.MinUnits > c.MaxUnits || c.MaxUnits > 1<<53 || !probability(c.PrefixFraction) {
		return fmt.Errorf("invalid compute range or prefix_fraction")
	}
	return nil
}

func (c ComputeConfig) Sample(rng *rand.Rand) (prefix, suffix uint64) {
	units := c.MinUnits + uint64(rng.Int63n(int64(c.MaxUnits-c.MinUnits+1)))
	prefix = uint64(float64(units) * c.PrefixFraction)
	return prefix, units - prefix
}

type KeySampler struct {
	size   int
	access AccessConfig
	cdf    []float64
}

func NewKeySampler(size int, access AccessConfig) KeySampler {
	s := KeySampler{size: size, access: access}
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

func (s *KeySampler) Key(rng *rand.Rand) int {
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

func (s *KeySampler) Keys(rng *rand.Rand, count int) []int {
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
			k = s.Key(rng)
		}
		if !seen[k] {
			keys = append(keys, k)
			seen[k] = true
		}
	}
	return keys
}
