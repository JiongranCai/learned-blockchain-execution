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

func TestSpeculationFormalTemplateRequiresFrozenLinuxControls(t *testing.T) {
	path := filepath.Join("..", "..", "configs", "experiments", "speculation-window", "linux-formal-template.json")
	if _, err := experiment.LoadConfig(path); !errors.Is(err, experiment.ErrInvalidConfig) {
		t.Fatalf("unfrozen Linux template was accepted: %v", err)
	}
}
