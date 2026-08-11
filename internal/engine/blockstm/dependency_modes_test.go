package blockstm_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/crypto-org-chain/go-block-stm/internal/control"
	engineapi "github.com/crypto-org-chain/go-block-stm/internal/engine"
	"github.com/crypto-org-chain/go-block-stm/internal/engine/blockstm"
	"github.com/crypto-org-chain/go-block-stm/internal/engine/serial"
	"github.com/crypto-org-chain/go-block-stm/internal/workload/synthetic"
)

func TestDependencyModesMatchSerialAcrossSeedsLimitsAndWorkers(t *testing.T) {
	controls := []struct {
		mode           control.DependencyMode
		information    control.DependencySource
		representation control.DependencyRepresentation
		builder        control.DependencyRepresentationBuilder
		wait           control.DependencyWaitPolicy
		estimates      control.DependencyEstimateInjection
	}{
		{control.DependencyMVCCRuntime, control.DependencySourceRuntimeObserved, control.DependencyRepresentationVersionOnly, control.DependencyRepresentationBuilderNone, control.DependencyWaitNone, control.DependencyEstimatesDisabled},
		{control.DependencyMVCCRuntime, control.DependencySourceStaticProgram, control.DependencyRepresentationVersionOnly, control.DependencyRepresentationBuilderNone, control.DependencyWaitNone, control.DependencyEstimatesDisabled},
		{control.DependencyMVCCRuntime, control.DependencySourceStaticProgram, control.DependencyRepresentationRAWLastWriter, control.DependencyRepresentationBuilderIndexedByKey, control.DependencyWaitNone, control.DependencyEstimatesDisabled},
		{control.DependencyMVCCRuntime, control.DependencySourceStaticProgram, control.DependencyRepresentationRAWLastWriter, control.DependencyRepresentationBuilderIndexedByKey, control.DependencyWaitDirectPredecessors, control.DependencyEstimatesDisabled},
		{control.DependencyMVCCRuntime, control.DependencySourceStaticProgram, control.DependencyRepresentationRAWLastWriter, control.DependencyRepresentationBuilderIndexedByKey, control.DependencyWaitNone, control.DependencyEstimatesWrite},
		{control.DependencyMVCCRuntime, control.DependencySourceStaticProgram, control.DependencyRepresentationMaxRAWPredecessor, control.DependencyRepresentationBuilderIndexedByKey, control.DependencyWaitContiguousFrontier, control.DependencyEstimatesDisabled},
		{control.DependencyMVCCRuntime, control.DependencySourceStaticProgram, control.DependencyRepresentationFullConflictGraph, control.DependencyRepresentationBuilderQuadraticReference, control.DependencyWaitNone, control.DependencyEstimatesDisabled},
		{control.DependencyMVCCRuntime, control.DependencySourceStaticProgram, control.DependencyRepresentationFullConflictGraph, control.DependencyRepresentationBuilderIndexedByKey, control.DependencyWaitAllPredecessors, control.DependencyEstimatesDisabled},
		{control.DependencyDeclaredDAG, control.DependencySourceStaticProgram, control.DependencyRepresentationRAWLastWriter, control.DependencyRepresentationBuilderIndexedByKey, control.DependencyWaitDirectPredecessors, control.DependencyEstimatesWrite},
		{control.DependencySummary, control.DependencySourceStaticProgram, control.DependencyRepresentationMaxRAWPredecessor, control.DependencyRepresentationBuilderIndexedByKey, control.DependencyWaitContiguousFrontier, control.DependencyEstimatesWrite},
		{control.DependencyFullGraph, control.DependencySourceStaticProgram, control.DependencyRepresentationFullConflictGraph, control.DependencyRepresentationBuilderQuadraticReference, control.DependencyWaitAllPredecessors, control.DependencyEstimatesWrite},
	}
	for _, shape := range []struct {
		name  string
		value string
	}{{"flat", ""}, {"state-dependent-branch", synthetic.ProgramShapeStateDependentBranch}} {
		for seed := int64(0); seed < 3; seed++ {
			artifact, err := synthetic.Generate(synthetic.Config{
				Seed:                 seed,
				InitialKeys:          8,
				KeySpace:             3,
				BlockCount:           2,
				TransactionsPerBlock: 20,
				MaxComputeUnits:      24,
				TransactionMaxUnits:  31,
				FailureEvery:         7,
				ProgramShape:         shape.value,
			})
			if err != nil {
				t.Fatal(err)
			}
			for _, workers := range []int{1, 4} {
				for _, limit := range []int{0, 3} {
					for _, dependency := range controls {
						name := fmt.Sprintf("%s/seed-%d/workers-%d/L-%d/%s/%s/%s/%s/%s/%s", shape.name, seed, workers, limit, dependency.mode, dependency.information, dependency.representation, dependency.builder, dependency.wait, dependency.estimates)
						t.Run(name, func(t *testing.T) {
							serialState, err := artifact.NewState()
							if err != nil {
								t.Fatal(err)
							}
							candidateState, err := artifact.NewState()
							if err != nil {
								t.Fatal(err)
							}
							for _, block := range artifact.OrderedBlocks {
								want, _, err := serial.New(nil).ExecuteBlock(
									context.Background(), block, serialState,
									engineapi.RunConfig{Executors: 1},
								)
								if err != nil {
									t.Fatal(err)
								}
								got, _, err := blockstm.New(nil).ExecuteBlock(
									context.Background(), block, candidateState,
									engineapi.RunConfig{
										Executors:                       workers,
										MaxSpeculativeInflight:          limit,
										DependencyMode:                  dependency.mode,
										DependencySource:                dependency.information,
										DependencyRepresentation:        dependency.representation,
										DependencyRepresentationBuilder: dependency.builder,
										DependencyWaitPolicy:            dependency.wait,
										DependencyEstimateInjection:     dependency.estimates,
									},
								)
								if err != nil {
									t.Fatal(err)
								}
								if !reflect.DeepEqual(want, got) {
									t.Fatalf("canonical result mismatch\nwant=%#v\ngot=%#v", want, got)
								}
							}
							if !reflect.DeepEqual(serialState.Snapshot(), candidateState.Snapshot()) {
								t.Fatalf("published state mismatch")
							}
						})
					}
				}
			}
		}
	}
}

func TestDependencyTelemetrySeparatesAcquisitionRepresentationAndUse(t *testing.T) {
	artifact, err := synthetic.Generate(synthetic.Config{
		Seed:                 17,
		InitialKeys:          4,
		KeySpace:             1,
		BlockCount:           1,
		TransactionsPerBlock: 32,
		MaxComputeUnits:      128,
		TransactionMaxUnits:  132,
	})
	if err != nil {
		t.Fatal(err)
	}
	block := artifact.OrderedBlocks[0]

	testCases := []struct {
		name               string
		mode               control.DependencyMode
		information        control.DependencySource
		representation     control.DependencyRepresentation
		builder            control.DependencyRepresentationBuilder
		wantAcquisition    bool
		wantRepresentation bool
		wantEdges          bool
		wantSummary        bool
		wantEstimates      bool
	}{
		{"runtime", control.DependencyMVCCRuntime, control.DependencySourceRuntimeObserved, control.DependencyRepresentationVersionOnly, control.DependencyRepresentationBuilderNone, false, false, false, false, false},
		{"equalized-version-only", control.DependencyMVCCRuntime, control.DependencySourceStaticProgram, control.DependencyRepresentationVersionOnly, control.DependencyRepresentationBuilderNone, true, false, false, false, false},
		{"representation-only-full-indexed", control.DependencyMVCCRuntime, control.DependencySourceStaticProgram, control.DependencyRepresentationFullConflictGraph, control.DependencyRepresentationBuilderIndexedByKey, true, true, true, false, false},
		{"declared-dag", control.DependencyDeclaredDAG, control.DependencySourceStaticProgram, control.DependencyRepresentationRAWLastWriter, control.DependencyRepresentationBuilderIndexedByKey, true, true, true, false, true},
		{"summary", control.DependencySummary, control.DependencySourceStaticProgram, control.DependencyRepresentationMaxRAWPredecessor, control.DependencyRepresentationBuilderIndexedByKey, true, true, false, true, true},
		{"full-graph", control.DependencyFullGraph, control.DependencySourceStaticProgram, control.DependencyRepresentationFullConflictGraph, control.DependencyRepresentationBuilderQuadraticReference, true, true, true, false, true},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			storage, err := artifact.NewState()
			if err != nil {
				t.Fatal(err)
			}
			_, trace, err := blockstm.New(nil).ExecuteBlock(
				context.Background(), block, storage,
				engineapi.RunConfig{
					Executors:                       8,
					DependencyMode:                  testCase.mode,
					DependencySource:                testCase.information,
					DependencyRepresentation:        testCase.representation,
					DependencyRepresentationBuilder: testCase.builder,
					TraceMode:                       control.TraceCounters,
				},
			)
			if err != nil {
				t.Fatal(err)
			}
			dependency := trace.Work.Dependency
			if dependency.Mode != testCase.mode || dependency.Source != testCase.information ||
				dependency.Representation != testCase.representation || dependency.RepresentationBuilder != testCase.builder ||
				!dependency.InformationComplete {
				t.Fatalf("identity/completeness mismatch: %#v", dependency)
			}
			if dependency.InformationExact != (testCase.information == control.DependencySourceRuntimeObserved) {
				t.Fatalf("information exactness mismatch: %#v", dependency)
			}
			if dependency.AcquisitionMeasured != testCase.wantAcquisition ||
				dependency.RepresentationMeasured != testCase.wantRepresentation {
				t.Fatalf("stage availability mismatch: %#v", dependency)
			}
			if (dependency.DependencyEdges > 0) != testCase.wantEdges ||
				(dependency.SummaryEntries > 0) != testCase.wantSummary ||
				(dependency.EstimatedWriteLocations > 0) != testCase.wantEstimates {
				t.Fatalf("representation counters mismatch: %#v", dependency)
			}
			if testCase.wantEstimates && dependency.EstimatedWriteKeyBytes == 0 {
				t.Fatalf("estimate key memory was not charged: %#v", dependency)
			}
			if testCase.wantAcquisition && (dependency.AcquisitionUnits == 0 || dependency.AcquisitionBytes == 0) {
				t.Fatalf("static acquisition was not charged: %#v", dependency)
			}
			if testCase.wantRepresentation && (dependency.RepresentationLogicalBytes == 0 ||
				dependency.RepresentationBuildUnits == 0) {
				t.Fatalf("representation/use was not charged: %#v", dependency)
			}
			if testCase.wantEstimates && (dependency.PlanLookups < uint64(len(block.Transactions)) ||
				dependency.TraversalSteps == 0 || dependency.ResolutionNS == 0 || dependency.EstimateBuildNS == 0) {
				t.Fatalf("static consumer was not charged: %#v", dependency)
			}
			if !testCase.wantEstimates && (dependency.PlanLookups != 0 || dependency.ResolutionMeasured || dependency.EstimateBuildNS != 0) {
				t.Fatalf("representation-only case reached a consumer: %#v", dependency)
			}
		})
	}
}

func TestEngineRejectsIllegalDependencyControls(t *testing.T) {
	artifact, err := synthetic.Generate(synthetic.Config{
		Seed: 1, InitialKeys: 1, KeySpace: 1, BlockCount: 1,
		TransactionsPerBlock: 1, TransactionMaxUnits: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, config := range []engineapi.RunConfig{
		{Executors: 1, DependencyMode: control.DependencySummary},
		{Executors: 1, DependencyMode: control.DependencySummary, DependencySource: control.DependencySourceRuntimeObserved},
		{Executors: 1, DependencyMode: control.DependencyMode("unknown"), DependencySource: control.DependencySourceStaticProgram},
		{Executors: 1, DependencyMode: control.DependencyMVCCRuntime, DependencySource: control.DependencySourceStaticProgram, DependencyRepresentation: control.DependencyRepresentationRAWLastWriter, DependencyRepresentationBuilder: control.DependencyRepresentationBuilderNone},
		{Executors: 1, DependencyMode: control.DependencySummary, DependencySource: control.DependencySourceStaticProgram, DependencyRepresentation: control.DependencyRepresentationFullConflictGraph, DependencyRepresentationBuilder: control.DependencyRepresentationBuilderIndexedByKey},
		{Executors: 1, DependencyMode: control.DependencyMVCCRuntime, DependencySource: control.DependencySourceStaticProgram, DependencyRepresentation: control.DependencyRepresentationRAWLastWriter, DependencyRepresentationBuilder: control.DependencyRepresentationBuilderIndexedByKey, DependencyWaitPolicy: control.DependencyWaitAllPredecessors, DependencyEstimateInjection: control.DependencyEstimatesDisabled},
		{Executors: 1, DependencyMode: control.DependencyMVCCRuntime, DependencySource: control.DependencySourceRuntimeObserved, DependencyRepresentation: control.DependencyRepresentationVersionOnly, DependencyRepresentationBuilder: control.DependencyRepresentationBuilderNone, DependencyWaitPolicy: control.DependencyWaitNone, DependencyEstimateInjection: control.DependencyEstimatesWrite},
		{Executors: 1, DependencyMode: control.DependencyDeclaredDAG, DependencySource: control.DependencySourceStaticProgram, DependencyRepresentation: control.DependencyRepresentationRAWLastWriter, DependencyRepresentationBuilder: control.DependencyRepresentationBuilderIndexedByKey, DependencyWaitPolicy: control.DependencyWaitDirectPredecessors, DependencyEstimateInjection: control.DependencyEstimatesDisabled},
	} {
		storage, stateErr := artifact.NewState()
		if stateErr != nil {
			t.Fatal(stateErr)
		}
		_, _, executeErr := blockstm.New(nil).ExecuteBlock(context.Background(), artifact.OrderedBlocks[0], storage, config)
		if !errors.Is(executeErr, engineapi.ErrInvalidDependencyMode) {
			t.Fatalf("illegal control returned %v", executeErr)
		}
	}
}
