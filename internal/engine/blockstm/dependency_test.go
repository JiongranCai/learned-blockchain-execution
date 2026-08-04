package blockstm

import (
	"context"
	"testing"
	"time"

	"github.com/crypto-org-chain/go-block-stm/internal/control"
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
		preparation, err := prepareDependency(
			context.Background(),
			block,
			mode,
			control.DependencyInformationStaticProgram,
			0,
			true,
		)
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
