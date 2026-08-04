package experiment_test

import (
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/crypto-org-chain/go-block-stm/internal/control"
	"github.com/crypto-org-chain/go-block-stm/internal/experiment"
)

func TestDependencyMatricesFreezeFourContrasts(t *testing.T) {
	want := map[string]control.DependencyInformation{
		string(control.DependencyMVCCRuntime): control.DependencyInformationRuntime,
		string(control.DependencyDeclaredDAG): control.DependencyInformationStaticProgram,
		string(control.DependencySummary):     control.DependencyInformationStaticProgram,
		string(control.DependencyFullGraph):   control.DependencyInformationStaticProgram,
	}
	for _, name := range []string{"cheap-hotspot-smoke.json", "expensive-low-conflict-smoke.json", "state-dependent-branch-smoke.json"} {
		t.Run(name, func(t *testing.T) {
			loaded := loadDependencyConfig(t, name)
			if _, err := experiment.LoadWorkload(loaded.Config.Workload); err != nil {
				t.Fatal(err)
			}
			seen := make(map[string]bool)
			for _, experimentCase := range loaded.Config.Cases {
				if experimentCase.Engine != "blockstm" || experimentCase.Executors != 8 || experimentCase.MaxSpeculativeInflight != 0 {
					t.Fatalf("case %s changes the frozen Block-STM/P=8/L=W controls", experimentCase.ID)
				}
				mode := string(experimentCase.DependencyMode)
				information, exists := want[mode]
				if !exists || experimentCase.DependencyInformation != information {
					t.Fatalf("case %s has unexpected dependency control %s/%s", experimentCase.ID, mode, experimentCase.DependencyInformation)
				}
				if seen[mode] {
					t.Fatalf("duplicate dependency mode %s", mode)
				}
				seen[mode] = true
			}
			if len(seen) != len(want) {
				t.Fatalf("got modes %v, want four frozen contrasts", seen)
			}
		})
	}
}

func TestDependencyEqualInformationAblationUsesOneStaticSource(t *testing.T) {
	loaded := loadDependencyConfig(t, "equal-information-smoke.json")
	wantModes := map[control.DependencyMode]bool{
		control.DependencyMVCCRuntime: false,
		control.DependencySummary:     false,
		control.DependencyFullGraph:   false,
	}
	for _, experimentCase := range loaded.Config.Cases {
		if experimentCase.DependencyInformation != control.DependencyInformationStaticProgram {
			t.Fatalf("case %s gets a different information source", experimentCase.ID)
		}
		if _, exists := wantModes[experimentCase.DependencyMode]; !exists {
			t.Fatalf("unexpected representation %s", experimentCase.DependencyMode)
		}
		wantModes[experimentCase.DependencyMode] = true
		if experimentCase.Executors != 8 || experimentCase.MaxSpeculativeInflight != 0 {
			t.Fatalf("case %s changes an unrelated control", experimentCase.ID)
		}
	}
	for mode, seen := range wantModes {
		if !seen {
			t.Fatalf("equal-information ablation omits %s", mode)
		}
	}
}

func TestSpeculationDependencyMatrixIsTwoByTwo(t *testing.T) {
	loaded := loadDependencyConfig(t, "speculation-interaction-smoke.json")
	want := map[string]bool{
		interactionKey(1, control.DependencyMVCCRuntime): false,
		interactionKey(0, control.DependencyMVCCRuntime): false,
		interactionKey(1, control.DependencyDeclaredDAG): false,
		interactionKey(0, control.DependencyDeclaredDAG): false,
	}
	for _, experimentCase := range loaded.Config.Cases {
		key := interactionKey(experimentCase.MaxSpeculativeInflight, experimentCase.DependencyMode)
		if _, exists := want[key]; !exists {
			t.Fatalf("unexpected interaction cell %s", key)
		}
		want[key] = true
		if experimentCase.Executors != 8 {
			t.Fatalf("case %s changes P=8", experimentCase.ID)
		}
		if experimentCase.DependencyMode == control.DependencyMVCCRuntime &&
			experimentCase.DependencyInformation != control.DependencyInformationRuntime {
			t.Fatalf("runtime cell %s does not use runtime information", experimentCase.ID)
		}
		if experimentCase.DependencyMode == control.DependencyDeclaredDAG &&
			experimentCase.DependencyInformation != control.DependencyInformationStaticProgram {
			t.Fatalf("guided cell %s does not use static program information", experimentCase.ID)
		}
	}
	for cell, seen := range want {
		if !seen {
			t.Fatalf("interaction matrix omits %s", cell)
		}
	}
}

func TestDependencyFormalTemplateRequiresFrozenLinuxControls(t *testing.T) {
	path := filepath.Join("..", "..", "configs", "experiments", "dependency-guidance", "linux-formal-template.json")
	if _, err := experiment.LoadConfig(path); !errors.Is(err, experiment.ErrInvalidConfig) {
		t.Fatalf("unfrozen Linux template was accepted: %v", err)
	}
}

func loadDependencyConfig(t *testing.T, name string) experiment.LoadedConfig {
	t.Helper()
	path := filepath.Join("..", "..", "configs", "experiments", "dependency-guidance", name)
	loaded, err := experiment.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	return loaded
}

func interactionKey(limit int, mode control.DependencyMode) string {
	return fmt.Sprintf("L=%d/%s", limit, mode)
}
