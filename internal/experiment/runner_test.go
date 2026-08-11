package experiment_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crypto-org-chain/go-block-stm/internal/control"
	"github.com/crypto-org-chain/go-block-stm/internal/experiment"
	"github.com/crypto-org-chain/go-block-stm/internal/telemetry"
	"github.com/crypto-org-chain/go-block-stm/internal/workload/synthetic"
)

const testArtifactHash = "6b71316d4076a8d0e27f078e6c52a2f9a042047e88c34ee2ea63792fafbe609d"

func TestValidateAndRunUseFrozenBundleAndVersionedTelemetry(t *testing.T) {
	loaded := writeTestConfig(t)
	bundle, err := experiment.Validate(context.Background(), loaded)
	if err != nil {
		t.Fatal(err)
	}
	if len(bundle.ValidatedCases) != 3 || bundle.ResultDigest == "" || bundle.WorkloadHash != testArtifactHash {
		t.Fatalf("unexpected validation bundle: %#v", bundle)
	}
	if bundle.ValidatedCases[0].MaxSpeculativeInflight != 2 || bundle.ValidatedCases[2].MaxSpeculativeInflight != 1 {
		t.Fatalf("validation bundle did not bind speculation limits: %#v", bundle.ValidatedCases)
	}
	if bundle.ValidatedCases[0].DependencyMode != control.DependencyMVCCRuntime ||
		bundle.ValidatedCases[0].DependencySource != control.DependencySourceRuntimeObserved {
		t.Fatalf("validation bundle did not bind dependency controls: %#v", bundle.ValidatedCases)
	}

	response, err := experiment.RunWorker(context.Background(), loaded, experiment.WorkerRequest{
		CaseID: "detailed",
		Phase:  "measurement",
		Round:  0,
		Order:  1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.Record.SchemaVersion != telemetry.BenchmarkRecordSchema || !response.Record.CanonicalMatch || response.Record.ResultDigest != bundle.ResultDigest {
		t.Fatalf("unexpected benchmark record: %#v", response.Record)
	}
	if response.Record.Case.MaxSpeculativeInflight != 1 || response.Record.Metrics.EffectiveSpeculationLimit != 1 ||
		!response.Record.Metrics.SpeculationLimitApplied || !response.Record.Metrics.SpeculationTelemetryAvailable {
		t.Fatalf("speculation case/metrics are incomplete: %#v", response.Record)
	}
	if response.Record.Case.DependencyMode != control.DependencyMVCCRuntime ||
		response.Record.Metrics.Dependency.Mode != control.DependencyMVCCRuntime ||
		response.Record.Metrics.Dependency.Source != control.DependencySourceRuntimeObserved {
		t.Fatalf("dependency case/metrics are incomplete: %#v", response.Record)
	}
	if response.Record.Provenance.ProcessID == 0 || len(response.Record.Provenance.BinarySHA256) != 64 ||
		len(response.Record.Capabilities.Events) != len(control.EventRegistry()) {
		t.Fatalf("runtime provenance/capabilities are incomplete: %#v", response.Record)
	}
	if len(response.Traces) != 3 || response.Traces[0].SchemaVersion != telemetry.ActionTraceSchema || response.Traces[0].BlockID != "block-000000" {
		t.Fatalf("unexpected action traces: %#v", response.Traces)
	}
	firstEvent := response.Traces[0].Trace.Events[0]
	if firstEvent.TrustClass == "" || firstEvent.FeatureSource != "fixed_config" || firstEvent.PolicyTableVersion == "" {
		t.Fatalf("action provenance is incomplete: %#v", firstEvent)
	}

	if err := experiment.RunMatrix(context.Background(), loaded, func(ctx context.Context, request experiment.WorkerRequest) (experiment.WorkerResponse, error) {
		return experiment.RunWorker(ctx, loaded, request)
	}); err != nil {
		t.Fatal(err)
	}
	runLines := nonEmptyLines(t, loaded.Config.Output.RunRecords)
	if len(runLines) != 4 || !strings.Contains(runLines[3], telemetry.AblationRecordSchema) {
		t.Fatalf("unexpected run JSONL: %v", runLines)
	}
	traceLines := nonEmptyLines(t, loaded.Config.Output.ActionTraces)
	if len(traceLines) != 3 {
		t.Fatalf("got %d trace lines, want 3", len(traceLines))
	}
}

func TestRunRejectsValidationBundleFromDifferentConfig(t *testing.T) {
	loaded := writeTestConfig(t)
	if _, err := experiment.Validate(context.Background(), loaded); err != nil {
		t.Fatal(err)
	}
	config := loaded.Config
	config.OrderSeed++
	writeJSONFile(t, loaded.Path, config)
	changed, err := experiment.LoadConfig(loaded.Path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = experiment.RunWorker(context.Background(), changed, experiment.WorkerRequest{
		CaseID: "off",
		Phase:  "measurement",
		Round:  0,
		Order:  0,
	})
	if !errors.Is(err, experiment.ErrInvalidValidationBundle) {
		t.Fatalf("got %v, want validation bundle error", err)
	}
}

func TestRunPreservesFailureRecord(t *testing.T) {
	loaded := writeTestConfig(t)
	if _, err := experiment.Validate(context.Background(), loaded); err != nil {
		t.Fatal(err)
	}
	wantErr := errors.New("worker crashed")
	err := experiment.RunMatrix(context.Background(), loaded, func(context.Context, experiment.WorkerRequest) (experiment.WorkerResponse, error) {
		return experiment.WorkerResponse{}, wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("got %v, want %v", err, wantErr)
	}
	lines := nonEmptyLines(t, loaded.Config.Output.RunRecords)
	if len(lines) != 1 || !strings.Contains(lines[0], `"status":"failed"`) || !strings.Contains(lines[0], "worker crashed") {
		t.Fatalf("failure record was not preserved: %v", lines)
	}
}

func TestConfigParserRejectsUnknownFieldsAndUnfrozenFormalEnvironment(t *testing.T) {
	directory := t.TempDir()
	unknownPath := filepath.Join(directory, "unknown.json")
	if err := os.WriteFile(unknownPath, []byte(`{"schema_version":"experiment-matrix-v6","unknown":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := experiment.LoadConfig(unknownPath); !errors.Is(err, experiment.ErrInvalidConfig) {
		t.Fatalf("unknown field: got %v", err)
	}

	loaded := writeTestConfig(t)
	config := loaded.Config
	config.RunClass = "formal"
	config.WarmupRounds = 3
	config.MeasurementRounds = 30
	config.Environment.Affinity = "REPLACE_WITH_CPUSET"
	writeJSONFile(t, loaded.Path, config)
	if _, err := experiment.LoadConfig(loaded.Path); !errors.Is(err, experiment.ErrInvalidConfig) {
		t.Fatalf("unfrozen formal environment: got %v", err)
	}

	loaded = writeTestConfig(t)
	config = loaded.Config
	config.Cases[0].MaxSpeculativeInflight = -1
	writeJSONFile(t, loaded.Path, config)
	if _, err := experiment.LoadConfig(loaded.Path); !errors.Is(err, experiment.ErrInvalidConfig) {
		t.Fatalf("negative speculation limit: got %v", err)
	}

	loaded = writeTestConfig(t)
	config = loaded.Config
	config.Cases[0].Engine = "serial"
	config.Cases[0].Policy = "serial_preset"
	config.Cases[0].MaxSpeculativeInflight = 2
	writeJSONFile(t, loaded.Path, config)
	if _, err := experiment.LoadConfig(loaded.Path); !errors.Is(err, experiment.ErrInvalidConfig) {
		t.Fatalf("serial speculation limit: got %v", err)
	}

	loaded = writeTestConfig(t)
	config = loaded.Config
	config.Cases[0].DependencyMode = control.DependencySummary
	config.Cases[0].DependencySource = control.DependencySourceRuntimeObserved
	writeJSONFile(t, loaded.Path, config)
	if _, err := experiment.LoadConfig(loaded.Path); !errors.Is(err, experiment.ErrInvalidConfig) {
		t.Fatalf("guided dependency mode without static information: got %v", err)
	}

	loaded = writeTestConfig(t)
	config = loaded.Config
	config.Cases[0].Engine = "serial"
	config.Cases[0].Policy = "serial_preset"
	config.Cases[0].MaxSpeculativeInflight = 1
	config.Cases[0].DependencySource = control.DependencySourceStaticProgram
	writeJSONFile(t, loaded.Path, config)
	if _, err := experiment.LoadConfig(loaded.Path); !errors.Is(err, experiment.ErrInvalidConfig) {
		t.Fatalf("serial dependency static information: got %v", err)
	}
}

func writeTestConfig(t *testing.T) experiment.LoadedConfig {
	t.Helper()
	directory := t.TempDir()
	protocolPath := filepath.Join(directory, "protocol.json")
	protocol := experiment.StatisticalProtocol{
		SchemaVersion:                experiment.StatisticalProtocolSchemaVersion,
		Status:                       "frozen",
		PairedWorkloadSeeds:          true,
		BalancedRandomizedOrder:      true,
		MinimumWarmupRounds:          1,
		MinimumMeasurementRounds:     2,
		ConfidenceLevel:              0.95,
		ConfidenceIntervalMethod:     "paired_percentile_bootstrap",
		BootstrapResamples:           100,
		MaterialEffectRatio:          0.05,
		TelemetryOverheadBudgetRatio: 0.50,
		P99MinimumTransactions:       100,
		MultipleComparisonCorrection: "holm",
		OutlierPolicy:                "retain",
		TimeoutPolicy:                "censor",
		CrashOOMPolicy:               "preserve",
		RankingReversalRequirements:  []string{"ci", "material", "nearby", "overhead"},
		PilotSeparation:              "separate",
	}
	writeJSONFile(t, protocolPath, protocol)

	configPath := filepath.Join(directory, "experiment.json")
	config := experiment.Config{
		SchemaVersion: experiment.ConfigSchemaVersion,
		RunClass:      "smoke",
		Workload: experiment.WorkloadConfig{
			Synthetic: &synthetic.Config{
				Seed:                 42,
				InitialKeys:          8,
				KeySpace:             6,
				BlockCount:           3,
				TransactionsPerBlock: 6,
				MaxComputeUnits:      8,
				TransactionMaxUnits:  12,
				FailureEvery:         3,
			},
			ExpectedHash: testArtifactHash,
		},
		StatisticalProtocol: protocolPath,
		WarmupRounds:        0,
		MeasurementRounds:   1,
		OrderSeed:           7,
		Timeout:             "30s",
		Environment: telemetry.Environment{
			Affinity:     "test",
			NUMAPolicy:   "test",
			StateReset:   "fresh_state_from_frozen_artifact",
			PageCache:    "test",
			ProcessReuse: "fresh_process_per_run",
		},
		Cases: []experiment.CaseConfig{
			{ID: "off", Engine: "blockstm", Policy: "blockstm_preset", Executors: 2, MaxSpeculativeInflight: 2, DependencyMode: control.DependencyMVCCRuntime, DependencySource: control.DependencySourceRuntimeObserved, DependencyRepresentation: control.DependencyRepresentationVersionOnly, DependencyRepresentationBuilder: control.DependencyRepresentationBuilderNone, DependencyWaitPolicy: control.DependencyWaitNone, DependencyEstimateInjection: control.DependencyEstimatesDisabled, TraceMode: control.TraceOff},
			{ID: "counters", Engine: "blockstm", Policy: "blockstm_preset", Executors: 2, MaxSpeculativeInflight: 2, DependencyMode: control.DependencyMVCCRuntime, DependencySource: control.DependencySourceRuntimeObserved, DependencyRepresentation: control.DependencyRepresentationVersionOnly, DependencyRepresentationBuilder: control.DependencyRepresentationBuilderNone, DependencyWaitPolicy: control.DependencyWaitNone, DependencyEstimateInjection: control.DependencyEstimatesDisabled, TraceMode: control.TraceCounters},
			{ID: "detailed", Engine: "blockstm", Policy: "blockstm_preset", Executors: 2, MaxSpeculativeInflight: 1, DependencyMode: control.DependencyMVCCRuntime, DependencySource: control.DependencySourceRuntimeObserved, DependencyRepresentation: control.DependencyRepresentationVersionOnly, DependencyRepresentationBuilder: control.DependencyRepresentationBuilderNone, DependencyWaitPolicy: control.DependencyWaitNone, DependencyEstimateInjection: control.DependencyEstimatesDisabled, TraceMode: control.TraceDetailed},
		},
		Output: experiment.OutputConfig{
			ValidationBundle:  filepath.Join(directory, "validation-bundle.json"),
			ValidationRecords: filepath.Join(directory, "validation.jsonl"),
			RunRecords:        filepath.Join(directory, "runs.jsonl"),
			ActionTraces:      filepath.Join(directory, "traces.jsonl"),
		},
		TelemetryAblation: &experiment.TelemetryAblationConfig{
			OffCase:          "off",
			InstrumentedCase: "counters",
		},
	}
	writeJSONFile(t, configPath, config)
	loaded, err := experiment.LoadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	return loaded
}

func writeJSONFile(t *testing.T, path string, value any) {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
}

func nonEmptyLines(t *testing.T, path string) []string {
	t.Helper()
	encoded, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	trimmed := strings.TrimSpace(string(encoded))
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "\n")
}
