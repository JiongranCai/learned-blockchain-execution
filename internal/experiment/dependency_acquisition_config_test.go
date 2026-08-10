package experiment_test

import (
	"path/filepath"
	"testing"

	"github.com/crypto-org-chain/go-block-stm/internal/control"
	"github.com/crypto-org-chain/go-block-stm/internal/experiment"
)

func TestCQ3IAcquisitionMatricesFreezeOtherDependencyControls(t *testing.T) {
	for _, name := range []string{"cheap-hotspot-smoke.json", "expensive-low-conflict-smoke.json"} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join("..", "..", "configs", "experiments", "dependency-acquisition", name)
			loaded, err := experiment.LoadConfig(path)
			if err != nil {
				t.Fatal(err)
			}
			if len(loaded.Config.Cases) != 2 {
				t.Fatalf("got %d CQ3-I cases, want 2", len(loaded.Config.Cases))
			}
			seen := map[control.DependencySource]bool{}
			for _, experimentCase := range loaded.Config.Cases {
				if experimentCase.Engine != "blockstm" || experimentCase.Executors != 8 ||
					experimentCase.MaxSpeculativeInflight != 0 ||
					experimentCase.DependencyMode != control.DependencyMVCCRuntime ||
					experimentCase.TraceMode != control.TraceCounters {
					t.Fatalf("case %s changes a frozen non-CQ3-I control", experimentCase.ID)
				}
				seen[experimentCase.DependencySource] = true
			}
			if !seen[control.DependencySourceRuntimeObserved] || !seen[control.DependencySourceStaticProgram] {
				t.Fatalf("CQ3-I sources are incomplete: %#v", seen)
			}
		})
	}
}

func TestCQ3ILinuxTemplateRequiresFrozenHostControls(t *testing.T) {
	path := filepath.Join("..", "..", "configs", "experiments", "dependency-acquisition", "linux-formal-template.json")
	if _, err := experiment.LoadConfig(path); err == nil {
		t.Fatal("unfrozen Linux template was accepted")
	}
}
