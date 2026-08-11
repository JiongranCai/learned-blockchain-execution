package experiment_test

import (
	"path/filepath"
	"testing"

	"github.com/crypto-org-chain/go-block-stm/internal/control"
	"github.com/crypto-org-chain/go-block-stm/internal/experiment"
)

func TestCQ3RRepresentationMatricesFreezeSourceAndConsumers(t *testing.T) {
	want := map[string]bool{
		representationKey(control.DependencyRepresentationVersionOnly, control.DependencyRepresentationBuilderNone):                     false,
		representationKey(control.DependencyRepresentationRAWLastWriter, control.DependencyRepresentationBuilderIndexedByKey):           false,
		representationKey(control.DependencyRepresentationMaxRAWPredecessor, control.DependencyRepresentationBuilderIndexedByKey):       false,
		representationKey(control.DependencyRepresentationFullConflictGraph, control.DependencyRepresentationBuilderQuadraticReference): false,
		representationKey(control.DependencyRepresentationFullConflictGraph, control.DependencyRepresentationBuilderIndexedByKey):       false,
	}
	for _, name := range []string{"cheap-hotspot-smoke.json", "expensive-low-conflict-smoke.json"} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join("..", "..", "configs", "experiments", "dependency-representation", name)
			loaded, err := experiment.LoadConfig(path)
			if err != nil {
				t.Fatal(err)
			}
			seen := make(map[string]bool, len(want))
			for _, experimentCase := range loaded.Config.Cases {
				if experimentCase.Engine != "blockstm" || experimentCase.Executors != 8 ||
					experimentCase.MaxSpeculativeInflight != 0 ||
					experimentCase.DependencyMode != control.DependencyMVCCRuntime ||
					experimentCase.DependencySource != control.DependencySourceStaticProgram ||
					experimentCase.DependencyWaitPolicy != control.DependencyWaitNone ||
					experimentCase.DependencyEstimateInjection != control.DependencyEstimatesDisabled ||
					experimentCase.TraceMode != control.TraceCounters {
					t.Fatalf("case %s changes a frozen non-CQ3-R control", experimentCase.ID)
				}
				key := representationKey(experimentCase.DependencyRepresentation, experimentCase.DependencyRepresentationBuilder)
				if _, exists := want[key]; !exists || seen[key] {
					t.Fatalf("unexpected or duplicate representation cell %s", key)
				}
				seen[key] = true
			}
			if len(seen) != len(want) {
				t.Fatalf("got CQ3-R cells %#v, want %#v", seen, want)
			}
		})
	}
}

func TestCQ3RLinuxTemplateRequiresFrozenHostControls(t *testing.T) {
	path := filepath.Join("..", "..", "configs", "experiments", "dependency-representation", "linux-formal-template.json")
	if _, err := experiment.LoadConfig(path); err == nil {
		t.Fatal("unfrozen CQ3-R Linux template was accepted")
	}
}

func representationKey(
	representation control.DependencyRepresentation,
	builder control.DependencyRepresentationBuilder,
) string {
	return string(representation) + "/" + string(builder)
}
