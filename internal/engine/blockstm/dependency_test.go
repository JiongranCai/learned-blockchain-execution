package blockstm

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/crypto-org-chain/go-block-stm/internal/control"
	engineapi "github.com/crypto-org-chain/go-block-stm/internal/engine"
	"github.com/crypto-org-chain/go-block-stm/internal/model"
)

func TestStaticDependencyRepresentationsShareAcquiredInformation(t *testing.T) {
	block := model.Block{ID: "representation", Transactions: []model.Transaction{
		dependencyTestTransaction("t0",
			model.Instruction{Op: model.OpRead, Key: []byte("a")},
			model.Instruction{Op: model.OpWrite, Key: []byte("b")},
		),
		dependencyTestTransaction("t1",
			model.Instruction{Op: model.OpRead, Key: []byte("b")},
			model.Instruction{Op: model.OpWrite, Key: []byte("c")},
		),
		dependencyTestTransaction("t2",
			model.Instruction{Op: model.OpRead, Key: []byte("a")},
			model.Instruction{Op: model.OpWrite, Key: []byte("b")},
		),
	}}

	accesses, acquisition, err := analyzeStaticPrograms(context.Background(), block)
	if err != nil {
		t.Fatal(err)
	}
	if !acquisition.complete || acquisition.units != 6 || acquisition.readKeys != 3 || acquisition.writeKeys != 3 {
		t.Fatalf("unexpected acquisition: %#v", acquisition)
	}

	dag, dagEdges, err := buildDeclaredRAWDAG(context.Background(), accesses)
	if err != nil {
		t.Fatal(err)
	}
	if dagEdges != 1 || len(dag[1]) != 1 || dag[1][0] != 0 {
		t.Fatalf("unexpected declared RAW DAG: edges=%d predecessors=%v", dagEdges, dag)
	}
	barriers, entries, err := buildDependencySummary(context.Background(), accesses)
	if err != nil {
		t.Fatal(err)
	}
	if entries != 1 || barriers[0] != -1 || barriers[1] != 0 || barriers[2] != -1 {
		t.Fatalf("unexpected summary: entries=%d barriers=%v", entries, barriers)
	}
	graph, graphEdges, err := buildFullConflictGraph(context.Background(), accesses)
	if err != nil {
		t.Fatal(err)
	}
	if graphEdges != 3 || len(graph[1]) != 1 || len(graph[2]) != 2 {
		t.Fatalf("unexpected full graph: edges=%d predecessors=%v", graphEdges, graph)
	}

	var acquired control.DependencyCounters
	for index, mode := range []control.DependencyMode{
		control.DependencyMVCCRuntime,
		control.DependencyDeclaredDAG,
		control.DependencySummary,
		control.DependencyFullGraph,
	} {
		plan, err := engineapi.EffectiveDependencyControl(engineapi.RunConfig{
			DependencyMode: mode, DependencySource: control.DependencySourceStaticProgram,
		})
		if err != nil {
			t.Fatal(err)
		}
		preparation, err := prepareDependency(context.Background(), block, plan, 0, true)
		if err != nil {
			t.Fatal(err)
		}
		if index == 0 {
			acquired = preparation.base
		} else if preparation.base.AcquisitionUnits != acquired.AcquisitionUnits ||
			preparation.base.AcquisitionBytes != acquired.AcquisitionBytes ||
			preparation.base.StaticReadKeys != acquired.StaticReadKeys ||
			preparation.base.StaticWriteKeys != acquired.StaticWriteKeys {
			t.Fatalf("mode %s acquired different information: got=%#v want=%#v", mode, preparation.base, acquired)
		}
		if !preparation.base.AcquisitionMeasured || !preparation.base.InformationComplete || preparation.base.InformationExact {
			t.Fatalf("unexpected static acquisition identity: %#v", preparation.base)
		}
		if mode == control.DependencyMVCCRuntime {
			if preparation.controller != nil || len(preparation.estimates) != 0 || preparation.base.RepresentationMeasured {
				t.Fatalf("equal-information version-only mode used static guidance: %#v", preparation)
			}
			continue
		}
		if len(preparation.estimates) != 3 || preparation.base.EstimatedWriteLocations != 3 ||
			!preparation.base.RepresentationMeasured || !preparation.base.ResolutionMeasured {
			t.Fatalf("unexpected prepared counters for %s: %#v", mode, preparation.base)
		}
	}
}

func TestCQ3IAcquisitionIsolation(t *testing.T) {
	block := model.Block{ID: "cq3-i", Transactions: []model.Transaction{
		dependencyTestTransaction("t0",
			model.Instruction{Op: model.OpRead, Key: []byte("a")},
			model.Instruction{Op: model.OpWrite, Key: []byte("b")},
		),
	}}

	runtime, err := prepareDependency(
		context.Background(), block, dependencyTestPlan(
			control.DependencyMVCCRuntime, control.DependencySourceRuntimeObserved,
			control.DependencyRepresentationVersionOnly, control.DependencyRepresentationBuilderNone,
		), 0, true,
	)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.base.SourceAvailableAt != control.DependencyAvailableDuringExecution ||
		runtime.base.SourceVersion != control.DependencySourceVersionRuntimeMVCC ||
		runtime.base.AcquisitionDisposition != control.DependencyDispositionRuntimeKernel ||
		!runtime.base.InformationComplete || !runtime.base.InformationExact ||
		runtime.base.AcquisitionMeasured {
		t.Fatalf("unexpected runtime acquisition: %#v", runtime.base)
	}

	static, err := prepareDependency(
		context.Background(), block, dependencyTestPlan(
			control.DependencyMVCCRuntime, control.DependencySourceStaticProgram,
			control.DependencyRepresentationVersionOnly, control.DependencyRepresentationBuilderNone,
		), 0, true,
	)
	if err != nil {
		t.Fatal(err)
	}
	if static.base.SourceAvailableAt != control.DependencyAvailableBeforeExecution ||
		static.base.SourceVersion != control.DependencySourceVersionStaticScan ||
		static.base.AcquisitionDisposition != control.DependencyDispositionDiscarded ||
		!static.base.InformationComplete || static.base.InformationExact ||
		!static.base.AcquisitionMeasured || static.base.AcquisitionUnits != 2 ||
		static.base.StaticReadKeys != 1 || static.base.StaticWriteKeys != 1 {
		t.Fatalf("unexpected static acquisition: %#v", static.base)
	}
	if static.controller != nil || len(static.estimates) != 0 ||
		static.base.RepresentationMeasured || static.base.ResolutionMeasured {
		t.Fatalf("CQ3-I static artifact escaped into representation/use: %#v", static)
	}
}

func TestCQ3RRepresentationIsolation(t *testing.T) {
	block := model.Block{ID: "cq3-r", Transactions: []model.Transaction{
		dependencyTestTransaction("t0", model.Instruction{Op: model.OpRead, Key: []byte("a")}, model.Instruction{Op: model.OpWrite, Key: []byte("b")}),
		dependencyTestTransaction("t1", model.Instruction{Op: model.OpRead, Key: []byte("b")}, model.Instruction{Op: model.OpWrite, Key: []byte("a")}),
		dependencyTestTransaction("t2", model.Instruction{Op: model.OpWrite, Key: []byte("b")}),
	}}
	testCases := []struct {
		name           string
		representation control.DependencyRepresentation
		builder        control.DependencyRepresentationBuilder
		wantEdges      bool
		wantSummary    bool
	}{
		{"version-only", control.DependencyRepresentationVersionOnly, control.DependencyRepresentationBuilderNone, false, false},
		{"raw-last-writer", control.DependencyRepresentationRAWLastWriter, control.DependencyRepresentationBuilderIndexedByKey, true, false},
		{"max-raw-predecessor", control.DependencyRepresentationMaxRAWPredecessor, control.DependencyRepresentationBuilderIndexedByKey, false, true},
		{"full-graph-reference", control.DependencyRepresentationFullConflictGraph, control.DependencyRepresentationBuilderQuadraticReference, true, false},
		{"full-graph-indexed", control.DependencyRepresentationFullConflictGraph, control.DependencyRepresentationBuilderIndexedByKey, true, false},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			preparation, err := prepareDependency(
				context.Background(), block,
				dependencyTestPlan(control.DependencyMVCCRuntime, control.DependencySourceStaticProgram, testCase.representation, testCase.builder),
				0, true,
			)
			if err != nil {
				t.Fatal(err)
			}
			counters := preparation.base
			if preparation.controller != nil || len(preparation.estimates) != 0 ||
				counters.ResolutionMeasured || counters.PlanLookups != 0 || counters.EstimatedWriteLocations != 0 {
				t.Fatalf("CQ3-R representation reached a static consumer: %#v", preparation)
			}
			if testCase.representation == control.DependencyRepresentationVersionOnly {
				if counters.RepresentationMeasured || counters.AcquisitionDisposition != control.DependencyDispositionDiscarded {
					t.Fatalf("unexpected version-only counters: %#v", counters)
				}
				return
			}
			if counters.AcquisitionDisposition != control.DependencyDispositionRepresentationDiscarded ||
				!counters.RepresentationMeasured || counters.RepresentationBuildUnits == 0 ||
				counters.RepresentationLogicalBytes == 0 || counters.RepresentationEntries == 0 {
				t.Fatalf("representation was not independently charged: %#v", counters)
			}
			if (counters.DependencyEdges > 0) != testCase.wantEdges ||
				(counters.SummaryEntries > 0) != testCase.wantSummary {
				t.Fatalf("unexpected representation topology: %#v", counters)
			}
		})
	}
}

func TestCQ3UConsumersAreIndependent(t *testing.T) {
	block := model.Block{ID: "cq3-u", Transactions: []model.Transaction{
		dependencyTestTransaction("t0", model.Instruction{Op: model.OpWrite, Key: []byte("a")}),
		dependencyTestTransaction("t1", model.Instruction{Op: model.OpRead, Key: []byte("a")}, model.Instruction{Op: model.OpWrite, Key: []byte("b")}),
		dependencyTestTransaction("t2", model.Instruction{Op: model.OpRead, Key: []byte("b")}),
	}}
	testCases := []struct {
		name           string
		representation control.DependencyRepresentation
		wait           control.DependencyWaitPolicy
		estimates      control.DependencyEstimateInjection
		wantGate       bool
		wantEstimates  bool
	}{
		{"raw-none", control.DependencyRepresentationRAWLastWriter, control.DependencyWaitNone, control.DependencyEstimatesDisabled, false, false},
		{"raw-direct-wait", control.DependencyRepresentationRAWLastWriter, control.DependencyWaitDirectPredecessors, control.DependencyEstimatesDisabled, true, false},
		{"raw-estimates", control.DependencyRepresentationRAWLastWriter, control.DependencyWaitNone, control.DependencyEstimatesWrite, false, true},
		{"summary-frontier-wait", control.DependencyRepresentationMaxRAWPredecessor, control.DependencyWaitContiguousFrontier, control.DependencyEstimatesDisabled, true, false},
		{"full-all-wait", control.DependencyRepresentationFullConflictGraph, control.DependencyWaitAllPredecessors, control.DependencyEstimatesDisabled, true, false},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			builder := control.DependencyRepresentationBuilderIndexedByKey
			plan, err := engineapi.EffectiveDependencyControl(engineapi.RunConfig{
				DependencyMode:                  control.DependencyMVCCRuntime,
				DependencySource:                control.DependencySourceStaticProgram,
				DependencyRepresentation:        testCase.representation,
				DependencyRepresentationBuilder: builder,
				DependencyWaitPolicy:            testCase.wait,
				DependencyEstimateInjection:     testCase.estimates,
			})
			if err != nil {
				t.Fatal(err)
			}
			preparation, err := prepareDependency(context.Background(), block, plan, 0, true)
			if err != nil {
				t.Fatal(err)
			}
			if (preparation.controller != nil) != testCase.wantGate ||
				(len(preparation.estimates) > 0) != testCase.wantEstimates {
				t.Fatalf("consumer coupling mismatch: %#v", preparation)
			}
			counters := preparation.Counters(2, 17)
			if counters.WaitPolicy != testCase.wait || counters.EstimateInjection != testCase.estimates {
				t.Fatalf("consumer identity mismatch: %#v", counters)
			}
			if testCase.wantGate != counters.ResolutionMeasured ||
				testCase.wantEstimates != (counters.EstimatedWriteLocations > 0) {
				t.Fatalf("consumer telemetry mismatch: %#v", counters)
			}
			if preparation.consumerActive && (counters.PostGuidanceReexecutions != 2 || counters.PostGuidanceReexecutionUnits != 17) {
				t.Fatalf("post-consumer work was not recorded: %#v", counters)
			}
			if !preparation.consumerActive && (counters.PostGuidanceReexecutions != 0 || counters.PostGuidanceReexecutionUnits != 0) {
				t.Fatalf("consumer-free plan recorded post-consumer work: %#v", counters)
			}
		})
	}
}

func TestIndexedFullConflictGraphMatchesQuadraticReference(t *testing.T) {
	accesses := []staticAccessSet{
		{reads: []string{"a"}, writes: []string{"b"}},
		{reads: []string{"b"}, writes: []string{"c"}},
		{reads: []string{"a", "c"}, writes: []string{"b"}},
		{writes: []string{"a"}},
		{reads: []string{"a", "b"}, writes: []string{"c"}},
	}
	want, wantEdges, err := buildFullConflictGraph(context.Background(), accesses)
	if err != nil {
		t.Fatal(err)
	}
	got, gotEdges, units, err := buildFullConflictGraphIndexed(context.Background(), accesses)
	if err != nil {
		t.Fatal(err)
	}
	if gotEdges != wantEdges || !reflect.DeepEqual(got, want) || units == 0 {
		t.Fatalf("indexed graph differs: got=%v/%d want=%v/%d units=%d", got, gotEdges, want, wantEdges, units)
	}
}

func TestDependencyPlanningHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := buildFullConflictGraph(ctx, make([]staticAccessSet, 1024)); err == nil {
		t.Fatal("cancelled full-graph construction succeeded")
	}
}

func TestExplicitDependencyGateWaitsForPredecessor(t *testing.T) {
	controller := &dependencyController{
		gate:    newExplicitDependencyGate([][]int{nil, []int{0}}),
		capture: true,
	}
	completed := make(chan error, 1)
	go func() { completed <- controller.Wait(context.Background(), 1) }()

	select {
	case err := <-completed:
		t.Fatalf("dependent returned before predecessor completion: %v", err)
	case <-time.After(10 * time.Millisecond):
	}
	controller.Complete(0)
	select {
	case err := <-completed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("dependent was not released")
	}
	if controller.waited.Load() != 1 || controller.waitEvents.Load() != 1 || controller.waitNS.Load() == 0 {
		t.Fatalf("wait telemetry was not recorded")
	}
}

func TestDependencyGateCancellationIsBounded(t *testing.T) {
	controller := &dependencyController{
		gate:    newSummaryDependencyGate([]int{-1, 0}),
		capture: true,
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := controller.Wait(ctx, 1); err == nil {
		t.Fatal("cancelled summary wait succeeded")
	}
}

func dependencyTestTransaction(id string, instructions ...model.Instruction) model.Transaction {
	return model.Transaction{ID: id, MaxUnits: 100, Program: model.Program{Instructions: instructions}}
}

func dependencyTestPlan(
	mode control.DependencyMode,
	source control.DependencySource,
	representation control.DependencyRepresentation,
	builder control.DependencyRepresentationBuilder,
) engineapi.DependencyPlan {
	plan, err := engineapi.EffectiveDependencyControl(engineapi.RunConfig{
		DependencyMode:                  mode,
		DependencySource:                source,
		DependencyRepresentation:        representation,
		DependencyRepresentationBuilder: builder,
	})
	if err != nil {
		panic(err)
	}
	return plan
}
