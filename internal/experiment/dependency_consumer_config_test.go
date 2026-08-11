package experiment_test

import (
	"path/filepath"
	"testing"

	"github.com/crypto-org-chain/go-block-stm/internal/control"
	"github.com/crypto-org-chain/go-block-stm/internal/experiment"
)

func TestCQ3UConsumerMatricesChangeOneConsumerAtATime(t *testing.T) {
	want := map[string]bool{
		consumerKey(control.DependencyRepresentationRAWLastWriter, control.DependencyWaitNone, control.DependencyEstimatesDisabled):                   false,
		consumerKey(control.DependencyRepresentationRAWLastWriter, control.DependencyWaitDirectPredecessors, control.DependencyEstimatesDisabled):     false,
		consumerKey(control.DependencyRepresentationRAWLastWriter, control.DependencyWaitNone, control.DependencyEstimatesWrite):                      false,
		consumerKey(control.DependencyRepresentationMaxRAWPredecessor, control.DependencyWaitNone, control.DependencyEstimatesDisabled):               false,
		consumerKey(control.DependencyRepresentationMaxRAWPredecessor, control.DependencyWaitContiguousFrontier, control.DependencyEstimatesDisabled): false,
		consumerKey(control.DependencyRepresentationFullConflictGraph, control.DependencyWaitNone, control.DependencyEstimatesDisabled):               false,
		consumerKey(control.DependencyRepresentationFullConflictGraph, control.DependencyWaitAllPredecessors, control.DependencyEstimatesDisabled):    false,
	}
	for _, name := range []string{"cheap-hotspot-smoke.json", "expensive-low-conflict-smoke.json"} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join("..", "..", "configs", "experiments", "dependency-consumer", name)
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
					experimentCase.DependencyRepresentationBuilder != control.DependencyRepresentationBuilderIndexedByKey ||
					experimentCase.TraceMode != control.TraceCounters {
					t.Fatalf("case %s changes a frozen non-CQ3-U control", experimentCase.ID)
				}
				key := consumerKey(experimentCase.DependencyRepresentation, experimentCase.DependencyWaitPolicy, experimentCase.DependencyEstimateInjection)
				if _, exists := want[key]; !exists || seen[key] {
					t.Fatalf("unexpected or duplicate consumer cell %s", key)
				}
				seen[key] = true
			}
			if len(seen) != len(want) {
				t.Fatalf("got CQ3-U cells %#v, want %#v", seen, want)
			}
		})
	}
}

func TestCQ3ULinuxTemplateRequiresFrozenHostControls(t *testing.T) {
	path := filepath.Join("..", "..", "configs", "experiments", "dependency-consumer", "linux-formal-template.json")
	if _, err := experiment.LoadConfig(path); err == nil {
		t.Fatal("unfrozen CQ3-U Linux template was accepted")
	}
}

func consumerKey(
	representation control.DependencyRepresentation,
	wait control.DependencyWaitPolicy,
	estimates control.DependencyEstimateInjection,
) string {
	return string(representation) + "/" + string(wait) + "/" + string(estimates)
}
