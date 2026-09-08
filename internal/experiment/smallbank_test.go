package experiment_test

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/crypto-org-chain/go-block-stm/internal/experiment"
	"github.com/crypto-org-chain/go-block-stm/internal/telemetry"
	"github.com/crypto-org-chain/go-block-stm/internal/workload/smallbank"
)

func TestSmallBankCQ3MatchesSerialWithFiniteWindows(t *testing.T) {
	loaded, err := experiment.LoadConfig(filepath.Join("..", "..", "configs", "experiments", "workload", "smallbank-smoke.json"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, seed := range []int64{131, 137} {
		for _, fraction := range []float64{0, 0.5, 0.9} {
			bank := loaded.Config.Workload.SmallBank
			bank.Seed = seed
			for i := range bank.Mix {
				bank.Mix[i].Compute.PrefixFraction = fraction
			}
			a, err := experiment.LoadWorkload(loaded.Config.Workload)
			if err != nil {
				t.Fatal(err)
			}
			oracleCase := loaded.Config.Cases[0]
			oracleCase.Engine, oracleCase.Policy = "serial", "serial_preset"
			oracleCase.Executors = 1
			oracleCase.EstimateReadPolicy, oracleCase.IdleWaitPolicy = "suspend_in_place", "gosched"
			oracle, err := experiment.Execute(ctx, a, oracleCase, false)
			if err != nil {
				t.Fatal(err)
			}
			for _, candidate := range loaded.Config.Cases {
				for _, limit := range []int{0, 1, 8, 32} {
					t.Run(fmt.Sprintf("%d/%.1f/%s/L%d", seed, fraction, candidate.ID, limit), func(t *testing.T) {
						candidate.MaxSpeculativeInflight = limit
						got, err := experiment.Execute(ctx, a, candidate, false)
						if err != nil {
							t.Fatal(err)
						}
						if !experiment.ResultsEqual(oracle.Results, got.Results) {
							t.Fatal("SmallBank differs from serial execution")
						}
						for _, trace := range got.Traces {
							if !trace.Work.KernelPolicy.Applied {
								t.Fatal("candidate did not use policy kernel")
							}
							if candidate.DependencySource == "static_program" && !trace.Work.Dependency.InformationComplete {
								t.Fatal("bank program was not fully scanned")
							}
						}
					})
				}
			}
		}
	}
}

func TestSmallBankSelectiveReadTelemetry(t *testing.T) {
	loaded, err := experiment.LoadConfig(filepath.Join("..", "..", "configs", "experiments", "workload", "smallbank-smoke.json"))
	if err != nil {
		t.Fatal(err)
	}
	bank := loaded.Config.Workload.SmallBank
	bank.Accounts = 1
	bank.InitialChecking = smallbank.BalanceRange{Min: 20, Max: 20}
	bank.InitialSavings = smallbank.BalanceRange{Min: 30, Max: 30}
	bank.BlockCount, bank.TransactionsPerBlock = 2, 16
	bank.Mix = []smallbank.TransactionConfig{
		{Type: smallbank.TransactSavings, Weight: 1, Amount: 1},
		{Type: smallbank.CheckFunds, Weight: 1, Amount: 20},
	}
	a, err := experiment.LoadWorkload(loaded.Config.Workload)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, candidate := range loaded.Config.Cases {
		got, err := experiment.Execute(ctx, a, candidate, false)
		if err != nil {
			t.Fatal(err)
		}
		metrics := telemetry.CollectMetrics(got.Results, got.Traces, 0, 0)
		if metrics.FinalReadOperations != 32 || metrics.FailedTransactions != 0 {
			t.Fatalf("%s: each final transaction must read exactly one key and succeed", candidate.ID)
		}
		if candidate.DependencySource == "static_program" && metrics.Dependency.StaticReadKeys <= metrics.FinalReadOperations {
			t.Fatalf("%s: static reads must include unexecuted Savings reads", candidate.ID)
		}
	}
}
