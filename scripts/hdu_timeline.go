// Diagnostic only: policy-hook timestamps are excluded from benchmark measurements.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/crypto-org-chain/go-block-stm/internal/control"
	engineapi "github.com/crypto-org-chain/go-block-stm/internal/engine"
	"github.com/crypto-org-chain/go-block-stm/internal/engine/blockstm"
	"github.com/crypto-org-chain/go-block-stm/internal/experiment"
	"github.com/crypto-org-chain/go-block-stm/internal/model"
	"github.com/crypto-org-chain/go-block-stm/internal/policy"
	"github.com/crypto-org-chain/go-block-stm/internal/policy/fixed"
	"github.com/crypto-org-chain/go-block-stm/internal/runtime/flat"
	"github.com/crypto-org-chain/go-block-stm/internal/state"
	"github.com/crypto-org-chain/go-block-stm/internal/state/memkv"
)

type event struct {
	Transaction string `json:"transaction"`
	Attempt     uint64 `json:"attempt"`
	Kind        string `json:"kind"`
	NS          int64  `json:"ns"`
}

type recorder struct {
	policy.Policy
	start  time.Time
	mu     sync.Mutex
	events []event
}

func (r *recorder) record(tx control.TxContext, kind string) {
	e := event{tx.TransactionID, tx.Incarnation, kind, time.Since(r.start).Nanoseconds()}
	r.mu.Lock()
	r.events = append(r.events, e)
	r.mu.Unlock()
}

func (r *recorder) OnTaskReady(c control.TaskContext) control.SchedulingDecision {
	r.record(c.TxContext, "start")
	return r.Policy.OnTaskReady(c)
}

func (r *recorder) BeforeRead(c control.AccessContext) control.AccessDecision {
	r.record(c.TxContext, "before_read")
	return r.Policy.BeforeRead(c)
}

func (r *recorder) OnValidationPoint(c control.ValidationContext) control.ValidationDecision {
	if c.Kind == control.ValidationTxEnd {
		r.record(c.TxContext, "tx_end") // Runtime finished; publication/validation follow.
	}
	return r.Policy.OnValidationPoint(c)
}

func (r *recorder) OnReplayStart(c control.ReplayContext) control.ReplayDecision {
	r.record(c.TxContext, c.Reason)
	return r.Policy.OnReplayStart(c)
}

func calibrate() any {
	const units = 20000000
	tx := model.Transaction{ID: "calibration", MaxUnits: units + 2,
		Program: model.Program{Instructions: []model.Instruction{
			{Op: model.OpCompute, ComputeUnits: units},
			{Op: model.OpReturn, Expression: model.Expression{Base: model.Literal(1)}},
		}}}
	var samples []float64
	for round := 0; round < 10; round++ {
		var workers sync.WaitGroup
		var elapsed [8]float64
		for i := range elapsed {
			workers.Add(1)
			go func(i int) {
				defer workers.Done()
				view := state.NewOverlay(memkv.New())
				start := time.Now()
				result := flat.New().Execute(context.Background(), uint64(i), tx, view)
				elapsed[i] = float64(time.Since(start).Nanoseconds()) / 1e6
				if result.Status != model.TxStatusSuccess {
					panic(result.ErrorCode)
				}
			}(i)
		}
		workers.Wait()
		if round >= 3 {
			samples = append(samples, elapsed[:]...)
		}
	}
	sorted := append([]float64(nil), samples...)
	sort.Float64s(sorted)
	median := (sorted[len(sorted)/2-1] + sorted[len(sorted)/2]) / 2
	return map[string]any{"compute_units": units, "workers": 8, "samples_ms": samples,
		"median_ms": median, "units_per_ms": int(float64(units)/median + 0.5)}
}

func execute(path, caseID string) (any, error) {
	loaded, err := experiment.LoadConfig(path)
	if err != nil {
		return nil, err
	}
	artifact, err := experiment.LoadWorkload(loaded.Config.Workload)
	if err != nil {
		return nil, err
	}
	if len(artifact.OrderedBlocks) != 1 {
		return nil, fmt.Errorf("timeline expects one block")
	}
	for _, c := range loaded.Config.Cases {
		if c.ID != caseID {
			continue
		}
		store, err := artifact.NewState()
		if err != nil {
			return nil, err
		}
		r := &recorder{Policy: fixed.NewBlockSTMPreset()}
		config := engineapi.RunConfig{Executors: c.Executors, Policy: r, TraceMode: control.TraceCounters,
			MaxSpeculativeInflight: c.MaxSpeculativeInflight, DependencyMode: c.DependencyMode,
			DependencySource: c.DependencySource, DependencyRepresentation: c.DependencyRepresentation,
			DependencyRepresentationBuilder: c.DependencyRepresentationBuilder,
			DependencyWaitPolicy:            c.DependencyWaitPolicy, DependencyEstimateInjection: c.DependencyEstimateInjection,
			DependencyDispatch: c.DependencyDispatch, EstimateReadPolicy: c.EstimateReadPolicy,
			IdleWaitPolicy: c.IdleWaitPolicy, OmitResultDigest: true}
		r.start = time.Now()
		result, trace, err := blockstm.New(nil).ExecuteBlock(context.Background(), artifact.OrderedBlocks[0], store, config)
		elapsed := time.Since(r.start).Nanoseconds()
		if err != nil {
			return nil, err
		}
		for _, tx := range result.Transactions {
			if tx.Status != model.TxStatusSuccess {
				return nil, fmt.Errorf("%s: %s", tx.TransactionID, tx.ErrorCode)
			}
		}
		sort.Slice(r.events, func(i, j int) bool { return r.events[i].NS < r.events[j].NS })
		return map[string]any{"case": caseID, "elapsed_ns": elapsed, "events": r.events, "work": trace.Work}, nil
	}
	return nil, fmt.Errorf("unknown case %q", caseID)
}

func main() {
	path := flag.String("config", "", "experiment matrix with one block")
	caseID := flag.String("case", "direct-ready", "case to observe")
	calibration := flag.Bool("calibrate", false, "measure CPU work with eight concurrent computations")
	flag.Parse()
	var result any
	var err error
	if *calibration {
		result = calibrate()
	} else {
		result, err = execute(*path, *caseID)
	}
	if err == nil {
		err = json.NewEncoder(os.Stdout).Encode(result)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
