package experiment_test

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/crypto-org-chain/go-block-stm/internal/experiment"
)

func TestSpeculationMatricesFreezeDistinctLimitsAndWorkloads(t *testing.T) {
	paths := []string{
		"expensive-low-conflict-smoke.json",
		"cheap-hotspot-smoke.json",
		"boundary-k1-smoke.json",
		"boundary-k2-smoke.json",
		"boundary-k3-smoke.json",
		"hotspot-cold-tail-smoke.json",
	}
	for _, name := range paths {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join("..", "..", "configs", "experiments", "speculation-window", name)
			loaded, err := experiment.LoadConfig(path)
			if err != nil {
				t.Fatal(err)
			}
			artifact, err := experiment.LoadWorkload(loaded.Config.Workload)
			if err != nil {
				t.Fatal(err)
			}
			if len(artifact.OrderedBlocks) == 0 {
				t.Fatal("speculation workload contains no blocks")
			}
			window := len(artifact.OrderedBlocks[0].Transactions)
			seen := make(map[int]string)
			blockSTMCount := 0
			for _, experimentCase := range loaded.Config.Cases {
				if experimentCase.Engine != "blockstm" {
					continue
				}
				blockSTMCount++
				if experimentCase.Executors != 8 {
					t.Fatalf("case %s changes the frozen P=8 worker pool", experimentCase.ID)
				}
				effective := experimentCase.MaxSpeculativeInflight
				if effective == 0 || effective > window {
					effective = window
				}
				if previous, exists := seen[effective]; exists {
					t.Fatalf("speculation limits collapse after min(L,W): %s and %s both use %d", previous, experimentCase.ID, effective)
				}
				seen[effective] = experimentCase.ID
			}
			if blockSTMCount != 4 || len(seen) != 4 {
				t.Fatalf("got %d Block-STM cases and %d distinct limits, want four", blockSTMCount, len(seen))
			}
		})
	}
}

func TestHotspotColdTailMatrixKeepsLargeKeySpaceAndExplicitDistribution(t *testing.T) {
	path := filepath.Join("..", "..", "configs", "experiments", "speculation-window", "hotspot-cold-tail-smoke.json")
	loaded, err := experiment.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	config := loaded.Config.Workload.Synthetic
	if config == nil || config.KeySpace != 8192 || config.AccessDistribution == nil {
		t.Fatalf("hotspot workload is incomplete: %#v", config)
	}
	distribution := config.AccessDistribution
	if distribution.Kind != "hotspot" || distribution.HotKeyCount != 8 || distribution.HotAccessProbability != 0.9 ||
		distribution.ReadWriteSameKeyProbability != 0.75 {
		t.Fatalf("unexpected hotspot distribution: %#v", distribution)
	}
	artifact, err := experiment.LoadWorkload(loaded.Config.Workload)
	if err != nil {
		t.Fatal(err)
	}
	if artifact.Generator.Version != "synthetic-v2" {
		t.Fatalf("got generator version %q", artifact.Generator.Version)
	}
}

func TestSpeculationFormalTemplateRequiresFrozenLinuxControls(t *testing.T) {
	path := filepath.Join("..", "..", "configs", "experiments", "speculation-window", "linux-formal-template.json")
	if _, err := experiment.LoadConfig(path); !errors.Is(err, experiment.ErrInvalidConfig) {
		t.Fatalf("unfrozen Linux template was accepted: %v", err)
	}
}
