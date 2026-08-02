package telemetry_test

import (
	"math"
	"testing"

	"github.com/crypto-org-chain/go-block-stm/internal/control"
	"github.com/crypto-org-chain/go-block-stm/internal/model"
	"github.com/crypto-org-chain/go-block-stm/internal/telemetry"
)

func TestCollectMetricsUsesCanonicalResultsAndTraceCounters(t *testing.T) {
	results := []model.BlockResult{{
		Transactions: []model.TxResult{
			{Status: model.TxStatusSuccess, UnitsUsed: 7},
			{Status: model.TxStatusFailed, UnitsUsed: 3},
		},
	}}
	traces := []control.Trace{{
		PolicyDecisionDurationNS: 11,
		WorkAvailable:            true,
		Work: control.WorkCounters{
			ExecutionAttempts:             3,
			ReexecutionAttempts:           1,
			UsefulExecutionUnits:          10,
			ReexecutedExecutionUnits:      4,
			DiscardedExecutionUnits:       4,
			SpeculationLimit:              2,
			SpeculationLimitApplied:       true,
			SpeculationTelemetryAvailable: true,
			PeakSpeculativeInflight:       2,
			AdmissionStallEvents:          3,
			AdmissionStallNS:              17,
		},
		ActionCounters: []control.ActionCounter{
			{Event: control.EventTxEnd, Action: "mandatory_final", Count: 2},
			{Event: control.EventValidationFail, Action: "whole_transaction", Count: 1},
			{Event: control.EventReplayStart, Action: "reexecute", Count: 1},
		},
		FallbackCounters: []control.ActionCounter{
			{Event: control.EventRetryLimit, Action: "serial", Count: 1},
		},
	}}
	metrics := telemetry.CollectMetrics(results, traces, 1_000_000_000, 4096)
	if metrics.Blocks != 1 || metrics.Transactions != 2 || metrics.SuccessfulTransactions != 1 || metrics.FailedTransactions != 1 {
		t.Fatalf("unexpected transaction metrics: %#v", metrics)
	}
	if metrics.UsefulExecutionUnits != 10 || metrics.ValidationEvents != 2 || metrics.ValidationFailures != 1 || metrics.ReexecutionEvents != 1 {
		t.Fatalf("unexpected work metrics: %#v", metrics)
	}
	if metrics.ExecutionAttempts != 3 || metrics.ReexecutionAttempts != 1 || metrics.ReexecutedExecutionUnits != 4 || metrics.DiscardedExecutionUnits != 4 {
		t.Fatalf("unexpected incarnation metrics: %#v", metrics)
	}
	if metrics.EffectiveSpeculationLimit != 2 || !metrics.SpeculationLimitApplied || !metrics.SpeculationTelemetryAvailable ||
		metrics.PeakSpeculativeInflight != 2 || metrics.AdmissionStallEvents != 3 || metrics.AdmissionStallNS != 17 {
		t.Fatalf("unexpected speculation metrics: %#v", metrics)
	}
	if metrics.CompletedTransactionsPerS != 2 || metrics.CommittedGoodputPerS != 1 || metrics.PolicyDecisionNS != 11 || metrics.MaxRSSBytes != 4096 {
		t.Fatalf("unexpected rate/provenance metrics: %#v", metrics)
	}
	if len(metrics.FallbackCounters) != 1 || len(metrics.Unavailable) == 0 {
		t.Fatalf("missing fallback or availability accounting: %#v", metrics)
	}
}

func TestTelemetryAblationUsesMediansAndFrozenBudget(t *testing.T) {
	record, err := telemetry.NewAblationRecord("off", "detailed", []uint64{100, 90, 110}, []uint64{105, 115, 110}, 0.10, true)
	if err != nil {
		t.Fatal(err)
	}
	if record.OffMedianNS != 100 || record.InstrumentedMedianNS != 110 || math.Abs(record.OverheadRatio-0.10) > 1e-12 || !record.WithinBudget {
		t.Fatalf("unexpected ablation: %#v", record)
	}
	if _, err := telemetry.NewAblationRecord("off", "detailed", nil, []uint64{1}, 0.10, false); err == nil {
		t.Fatal("empty ablation samples were accepted")
	}
}
