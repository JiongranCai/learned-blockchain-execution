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
	"github.com/crypto-org-chain/go-block-stm/internal/workload"
	"github.com/crypto-org-chain/go-block-stm/internal/workload/synthetic"
)

// kernelPolicyCase enumerates the additive kernel policies. Every one of them
// only changes when work happens, so all of them must reproduce the preset
// order serial oracle exactly.
type kernelPolicyCase struct {
	name         string
	estimateRead control.EstimateReadPolicy
	idleWait     control.IdleWaitPolicy
	dispatch     control.DependencyDispatchPolicy
}

func kernelPolicyCases() []kernelPolicyCase {
	return []kernelPolicyCase{
		{"frozen-default", "", "", ""},
		{"park-idle", "", control.IdleWaitPark, ""},
		{"abort-reschedule", control.EstimateReadAbortReschedule, "", ""},
		{"abort-reschedule-park", control.EstimateReadAbortReschedule, control.IdleWaitPark, ""},
		{"suspend-yield-worker", control.EstimateReadSuspendYieldWorker, "", ""},
	}
}

// dependency consumers that can be driven by either dispatch policy.
type kernelDependencyCase struct {
	name           string
	source         control.DependencySource
	representation control.DependencyRepresentation
	builder        control.DependencyRepresentationBuilder
	wait           control.DependencyWaitPolicy
	estimates      control.DependencyEstimateInjection
	gated          bool
}

func kernelDependencyCases() []kernelDependencyCase {
	return []kernelDependencyCase{
		{"runtime", control.DependencySourceRuntimeObserved, control.DependencyRepresentationVersionOnly, control.DependencyRepresentationBuilderNone, control.DependencyWaitNone, control.DependencyEstimatesDisabled, false},
		{"estimates", control.DependencySourceStaticProgram, control.DependencyRepresentationRAWLastWriter, control.DependencyRepresentationBuilderIndexedByKey, control.DependencyWaitNone, control.DependencyEstimatesWrite, false},
		{"direct-wait", control.DependencySourceStaticProgram, control.DependencyRepresentationRAWLastWriter, control.DependencyRepresentationBuilderIndexedByKey, control.DependencyWaitDirectPredecessors, control.DependencyEstimatesDisabled, true},
		{"frontier-wait", control.DependencySourceStaticProgram, control.DependencyRepresentationMaxRAWPredecessor, control.DependencyRepresentationBuilderIndexedByKey, control.DependencyWaitContiguousFrontier, control.DependencyEstimatesDisabled, true},
		{"all-wait", control.DependencySourceStaticProgram, control.DependencyRepresentationFullConflictGraph, control.DependencyRepresentationBuilderIndexedByKey, control.DependencyWaitAllPredecessors, control.DependencyEstimatesDisabled, true},
	}
}

func TestKernelPoliciesMatchSerialOracle(t *testing.T) {
	shapes := []struct {
		name  string
		value string
	}{{"flat", ""}, {"state-dependent-branch", synthetic.ProgramShapeStateDependentBranch}}

	for _, shape := range shapes {
		for seed := int64(0); seed < 2; seed++ {
			artifact, err := synthetic.Generate(synthetic.Config{
				Seed:                 seed,
				InitialKeys:          8,
				KeySpace:             3,
				BlockCount:           2,
				TransactionsPerBlock: 24,
				MaxComputeUnits:      24,
				TransactionMaxUnits:  31,
				FailureEvery:         7,
				ProgramShape:         shape.value,
			})
			if err != nil {
				t.Fatal(err)
			}
			for _, policy := range kernelPolicyCases() {
				for _, dependency := range kernelDependencyCases() {
					for _, workers := range []int{1, 4} {
						for _, dispatch := range []control.DependencyDispatchPolicy{"", control.DependencyDispatchReadyQueue} {
							if dispatch == control.DependencyDispatchReadyQueue && !dependency.gated {
								continue
							}
							name := fmt.Sprintf("%s/seed-%d/%s/%s/dispatch-%s/workers-%d",
								shape.name, seed, policy.name, dependency.name, dispatchLabel(dispatch), workers)
							t.Run(name, func(t *testing.T) {
								config := engineapi.RunConfig{
									Executors:                       workers,
									DependencyMode:                  control.DependencyMVCCRuntime,
									DependencySource:                dependency.source,
									DependencyRepresentation:        dependency.representation,
									DependencyRepresentationBuilder: dependency.builder,
									DependencyWaitPolicy:            dependency.wait,
									DependencyEstimateInjection:     dependency.estimates,
									DependencyDispatch:              dispatch,
									EstimateReadPolicy:              policy.estimateRead,
									IdleWaitPolicy:                  policy.idleWait,
								}
								assertMatchesSerialOracle(t, artifact, config)
							})
						}
					}
				}
			}
		}
	}
}

func dispatchLabel(policy control.DependencyDispatchPolicy) string {
	if policy == "" {
		return "index_order"
	}
	return string(policy)
}

// assertMatchesSerialOracle runs the preset-order serial oracle and the
// candidate configuration on independent clones of the same initial state and
// requires complete canonical equality of every block result and of the final
// published state.
func assertMatchesSerialOracle(t *testing.T, artifact workload.Artifact, config engineapi.RunConfig) {
	t.Helper()
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
			context.Background(), block, serialState, engineapi.RunConfig{Executors: 1},
		)
		if err != nil {
			t.Fatal(err)
		}
		got, trace, err := blockstm.New(nil).ExecuteBlock(
			context.Background(), block, candidateState, config,
		)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(want, got) {
			t.Fatalf("canonical result mismatch\nwant=%#v\ngot=%#v", want, got)
		}
		assertKernelPolicyTelemetry(t, config, trace)
	}
	if !reflect.DeepEqual(serialState.Snapshot(), candidateState.Snapshot()) {
		t.Fatal("published state mismatch")
	}
}

// assertKernelPolicyTelemetry checks that a record always states which kernel
// behaviour produced it, and that ready-queue dispatch really replaced the
// in-callback wait instead of running both.
func assertKernelPolicyTelemetry(t *testing.T, config engineapi.RunConfig, trace control.Trace) {
	t.Helper()
	counters := trace.Work.KernelPolicy
	plan, err := engineapi.EffectiveKernelControl(config, engineapi.DependencyPlan{WaitPolicy: config.DependencyWaitPolicy})
	if err != nil {
		t.Fatal(err)
	}
	if counters.EstimateRead != plan.EstimateRead ||
		counters.IdleWait != plan.IdleWait ||
		counters.Dispatch != plan.Dispatch {
		t.Fatalf("kernel policy telemetry does not match the resolved plan: %#v vs %#v", counters, plan)
	}
	if counters.Applied == plan.IsFrozenDefault() {
		t.Fatalf("frozen default must not report an applied policy kernel: %#v", counters)
	}
	if plan.Dispatch == control.DependencyDispatchReadyQueue {
		if trace.Work.Dependency.PlanLookups != 0 || trace.Work.Dependency.WaitNS != 0 {
			t.Fatalf("ready_queue dispatch must not also run the in-callback gate: %#v", trace.Work.Dependency)
		}
	}
	if plan.EstimateRead != control.EstimateReadAbortReschedule && counters.EstimateAborts != 0 {
		t.Fatalf("only abort_and_reschedule may abort on an estimate read: %#v", counters)
	}
	if plan.EstimateRead != control.EstimateReadSuspendYieldWorker && counters.WorkerYields != 0 {
		t.Fatalf("only suspend_yield_worker may yield a worker: %#v", counters)
	}
}

func TestKernelPolicyRejectsUnsupportedCombinations(t *testing.T) {
	base := engineapi.RunConfig{
		DependencyMode:   control.DependencyMVCCRuntime,
		DependencySource: control.DependencySourceRuntimeObserved,
	}

	plan, err := engineapi.EffectiveDependencyControl(base)
	if err != nil {
		t.Fatal(err)
	}

	// ready_queue without a wait consumer has nothing to gate on.
	config := base
	config.DependencyDispatch = control.DependencyDispatchReadyQueue
	if _, err := engineapi.EffectiveKernelControl(config, plan); !errors.Is(err, engineapi.ErrInvalidKernelPolicy) {
		t.Fatalf("expected ready_queue without wait consumer to be rejected, got %v", err)
	}

	// A finite speculation window is not composed with a policy kernel.
	config = base
	config.IdleWaitPolicy = control.IdleWaitPark
	config.MaxSpeculativeInflight = 3
	if _, err := engineapi.EffectiveKernelControl(config, plan); !errors.Is(err, engineapi.ErrInvalidKernelPolicy) {
		t.Fatalf("expected finite speculation window to be rejected, got %v", err)
	}

	// Unknown values are rejected rather than silently defaulted.
	config = base
	config.EstimateReadPolicy = "nope"
	if _, err := engineapi.EffectiveKernelControl(config, plan); !errors.Is(err, engineapi.ErrInvalidKernelPolicy) {
		t.Fatalf("expected unknown estimate read policy to be rejected, got %v", err)
	}

	// suspend_yield_worker forces parked idle waiting.
	config = base
	config.EstimateReadPolicy = control.EstimateReadSuspendYieldWorker
	resolved, err := engineapi.EffectiveKernelControl(config, plan)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.IdleWait != control.IdleWaitPark {
		t.Fatalf("suspend_yield_worker must force parked idle waiting, got %q", resolved.IdleWait)
	}
	if resolved.IsFrozenDefault() {
		t.Fatal("suspend_yield_worker must not resolve to the frozen default")
	}

	// An omitted policy resolves to the non-blocking behaviour when it is
	// available: abort_and_reschedule frees the worker of a transaction that
	// cannot proceed, and park keeps freed workers from spinning.
	resolved, err = engineapi.EffectiveKernelControl(base, plan)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.EstimateRead != control.EstimateReadAbortReschedule ||
		resolved.IdleWait != control.IdleWaitPark {
		t.Fatalf("omitted policy must resolve to the non-blocking default, got %#v", resolved)
	}
	// ready_queue needs a wait consumer, so an omitted dispatch stays in index
	// order for a plan that has none, and becomes a ready queue for one that does.
	if resolved.Dispatch != control.DependencyDispatchIndexOrder {
		t.Fatalf("omitted dispatch without a wait consumer must stay in index order, got %#v", resolved)
	}
	gated, err := engineapi.EffectiveDependencyControl(engineapi.RunConfig{
		DependencyMode:                  control.DependencyMVCCRuntime,
		DependencySource:                control.DependencySourceStaticProgram,
		DependencyRepresentation:        control.DependencyRepresentationRAWLastWriter,
		DependencyRepresentationBuilder: control.DependencyRepresentationBuilderIndexedByKey,
		DependencyWaitPolicy:            control.DependencyWaitDirectPredecessors,
		DependencyEstimateInjection:     control.DependencyEstimatesDisabled,
	})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err = engineapi.EffectiveKernelControl(base, gated)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Dispatch != control.DependencyDispatchReadyQueue {
		t.Fatalf("omitted dispatch with a wait consumer must resolve to a ready queue, got %#v", resolved)
	}

	// A finite speculation window has no policy scheduler, so an omitted policy
	// falls back to the frozen upstream kernel rather than failing.
	windowed := base
	windowed.MaxSpeculativeInflight = 4
	resolved, err = engineapi.EffectiveKernelControl(windowed, gated)
	if err != nil {
		t.Fatal(err)
	}
	if !resolved.IsFrozenDefault() {
		t.Fatalf("a finite window must fall back to the frozen kernel, got %#v", resolved)
	}
	// An explicit non-frozen policy is refused rather than downgraded.
	windowed.EstimateReadPolicy = control.EstimateReadAbortReschedule
	if _, err = engineapi.EffectiveKernelControl(windowed, gated); !errors.Is(err, engineapi.ErrInvalidKernelPolicy) {
		t.Fatalf("explicit policy with a finite window must be refused, got %v", err)
	}
}

// TestAbortedAttemptIsAccountedBeforeRedispatch pins the estimate-abort
// ordering and accounting. A discarded attempt must be charged the work it
// consumed and must not be reported as a validation failure, which only holds
// if the attempt finishes unwinding before the transaction becomes eligible
// again.
func TestAbortedAttemptIsAccountedBeforeRedispatch(t *testing.T) {
	artifact, err := synthetic.Generate(synthetic.Config{
		Seed: 3, InitialKeys: 64, KeySpace: 2, BlockCount: 1,
		TransactionsPerBlock: 128, MaxComputeUnits: 512, MinComputeUnits: 512,
		TransactionMaxUnits: 1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	block := artifact.OrderedBlocks[0]
	storage, err := artifact.NewState()
	if err != nil {
		t.Fatal(err)
	}
	_, trace, err := blockstm.New(nil).ExecuteBlock(
		context.Background(), block, storage,
		engineapi.RunConfig{
			Executors:                       8,
			DependencyMode:                  control.DependencyMVCCRuntime,
			DependencySource:                control.DependencySourceStaticProgram,
			DependencyRepresentation:        control.DependencyRepresentationRAWLastWriter,
			DependencyRepresentationBuilder: control.DependencyRepresentationBuilderIndexedByKey,
			DependencyWaitPolicy:            control.DependencyWaitNone,
			DependencyEstimateInjection:     control.DependencyEstimatesWrite,
			EstimateReadPolicy:              control.EstimateReadAbortReschedule,
			TraceMode:                       control.TraceCounters,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	work := trace.Work
	if work.KernelPolicy.EstimateAborts == 0 {
		t.Fatal("workload produced no estimate aborts; the assertions below prove nothing")
	}
	if work.DiscardedExecutionUnits == 0 {
		t.Fatalf("discarded attempts were charged no work: %#v", work)
	}
	for _, counter := range trace.ActionCounters {
		if counter.Event == control.EventValidationFail && counter.Count > 0 {
			t.Fatalf("an estimate abort was reported as a validation failure: %#v", counter)
		}
	}
}
