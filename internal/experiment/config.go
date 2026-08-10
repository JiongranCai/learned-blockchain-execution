package experiment

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/crypto-org-chain/go-block-stm/internal/control"
	"github.com/crypto-org-chain/go-block-stm/internal/policy/fixed"
	"github.com/crypto-org-chain/go-block-stm/internal/telemetry"
	"github.com/crypto-org-chain/go-block-stm/internal/workload/synthetic"
)

const ConfigSchemaVersion = "experiment-matrix-v4"

var ErrInvalidConfig = errors.New("invalid experiment config")

type Config struct {
	SchemaVersion       string                   `json:"schema_version"`
	RunClass            string                   `json:"run_class"`
	Workload            WorkloadConfig           `json:"workload"`
	StatisticalProtocol string                   `json:"statistical_protocol"`
	WarmupRounds        int                      `json:"warmup_rounds"`
	MeasurementRounds   int                      `json:"measurement_rounds"`
	OrderSeed           int64                    `json:"order_seed"`
	Timeout             string                   `json:"timeout"`
	Environment         telemetry.Environment    `json:"environment"`
	Cases               []CaseConfig             `json:"cases"`
	Output              OutputConfig             `json:"output"`
	TelemetryAblation   *TelemetryAblationConfig `json:"telemetry_ablation,omitempty"`
}

type WorkloadConfig struct {
	ArtifactPath string            `json:"artifact_path,omitempty"`
	Synthetic    *synthetic.Config `json:"synthetic,omitempty"`
	ExpectedHash string            `json:"expected_hash"`
}

type CaseConfig struct {
	ID                     string                   `json:"id"`
	Engine                 string                   `json:"engine"`
	Policy                 string                   `json:"policy"`
	Executors              int                      `json:"executors"`
	MaxSpeculativeInflight int                      `json:"max_speculative_inflight"`
	DependencyMode         control.DependencyMode   `json:"dependency_mode"`
	DependencySource       control.DependencySource `json:"dependency_source"`
	TraceMode              control.TraceMode        `json:"trace_mode"`
}

func (c CaseConfig) TelemetryCase() telemetry.Case {
	return telemetry.Case{
		ID:                     c.ID,
		Engine:                 c.Engine,
		Policy:                 c.Policy,
		PolicyVersion:          fixed.PresetVersion,
		Executors:              c.Executors,
		MaxSpeculativeInflight: c.MaxSpeculativeInflight,
		DependencyMode:         c.DependencyMode,
		DependencySource:       c.DependencySource,
		TraceMode:              c.TraceMode,
	}
}

type OutputConfig struct {
	ValidationBundle  string `json:"validation_bundle"`
	ValidationRecords string `json:"validation_records"`
	RunRecords        string `json:"run_records"`
	ActionTraces      string `json:"action_traces"`
}

type TelemetryAblationConfig struct {
	OffCase          string   `json:"off_case"`
	InstrumentedCase string   `json:"instrumented_case"`
	EnforcePlatforms []string `json:"enforce_platforms"`
}

type LoadedConfig struct {
	Config  Config
	Hash    string
	Path    string
	Timeout time.Duration
}

func LoadConfig(path string) (LoadedConfig, error) {
	encoded, err := os.ReadFile(path)
	if err != nil {
		return LoadedConfig{}, err
	}
	var config Config
	if err := decodeStrict(encoded, &config); err != nil {
		return LoadedConfig{}, fmt.Errorf("%w: %v", ErrInvalidConfig, err)
	}
	timeout, err := config.validate()
	if err != nil {
		return LoadedConfig{}, err
	}
	canonical, err := json.Marshal(config)
	if err != nil {
		return LoadedConfig{}, err
	}
	digest := sha256.Sum256(append([]byte(ConfigSchemaVersion+"\x00"), canonical...))
	return LoadedConfig{
		Config:  config,
		Hash:    hex.EncodeToString(digest[:]),
		Path:    path,
		Timeout: timeout,
	}, nil
}

func SchemaHash(schemaVersion string) string {
	digest := sha256.Sum256([]byte(schemaVersion))
	return hex.EncodeToString(digest[:])
}

func (c Config) validate() (time.Duration, error) {
	invalid := func(format string, args ...any) (time.Duration, error) {
		return 0, fmt.Errorf("%w: %s", ErrInvalidConfig, fmt.Sprintf(format, args...))
	}
	if c.SchemaVersion != ConfigSchemaVersion {
		return invalid("schema_version must be %q", ConfigSchemaVersion)
	}
	if c.RunClass != "smoke" && c.RunClass != "formal" {
		return invalid("run_class must be smoke or formal")
	}
	if (c.Workload.ArtifactPath == "") == (c.Workload.Synthetic == nil) {
		return invalid("workload must set exactly one of artifact_path or synthetic")
	}
	if len(c.Workload.ExpectedHash) != sha256.Size*2 {
		return invalid("workload expected_hash must be a SHA-256 hex digest")
	}
	if _, err := hex.DecodeString(c.Workload.ExpectedHash); err != nil {
		return invalid("workload expected_hash: %v", err)
	}
	if c.StatisticalProtocol == "" {
		return invalid("statistical_protocol is required")
	}
	if c.WarmupRounds < 0 || c.MeasurementRounds <= 0 {
		return invalid("warmup_rounds must be non-negative and measurement_rounds positive")
	}
	timeout, err := time.ParseDuration(c.Timeout)
	if err != nil || timeout <= 0 {
		return invalid("timeout must be a positive Go duration")
	}
	if c.Environment.Affinity == "" || c.Environment.NUMAPolicy == "" || c.Environment.StateReset == "" ||
		c.Environment.PageCache == "" || c.Environment.ProcessReuse == "" {
		return invalid("all environment controls must be explicit")
	}
	if c.Environment.ProcessReuse != "fresh_process_per_run" {
		return invalid("process_reuse must be fresh_process_per_run")
	}
	if c.Environment.StateReset != "fresh_state_from_frozen_artifact" {
		return invalid("state_reset must be fresh_state_from_frozen_artifact")
	}
	if c.RunClass == "formal" {
		controls := []string{c.Environment.Affinity, c.Environment.NUMAPolicy, c.Environment.PageCache}
		for _, value := range controls {
			if strings.Contains(value, "REPLACE_WITH_") || strings.Contains(value, "uncontrolled") {
				return invalid("formal environment controls must be frozen before execution")
			}
		}
	}
	if len(c.Cases) == 0 {
		return invalid("at least one case is required")
	}
	caseIDs := make(map[string]CaseConfig, len(c.Cases))
	for _, experimentCase := range c.Cases {
		if experimentCase.ID == "" || experimentCase.Engine == "" || experimentCase.Policy == "" {
			return invalid("case id, engine, and policy are required")
		}
		if _, exists := caseIDs[experimentCase.ID]; exists {
			return invalid("duplicate case id %q", experimentCase.ID)
		}
		if experimentCase.Executors < 0 {
			return invalid("case %q has a negative executor count", experimentCase.ID)
		}
		if experimentCase.MaxSpeculativeInflight < 0 {
			return invalid("case %q has a negative max_speculative_inflight", experimentCase.ID)
		}
		if !control.ValidDependencyMode(experimentCase.DependencyMode) {
			return invalid("case %q has invalid dependency_mode %q", experimentCase.ID, experimentCase.DependencyMode)
		}
		if !control.ValidDependencySource(experimentCase.DependencySource) {
			return invalid("case %q has invalid dependency_source %q", experimentCase.ID, experimentCase.DependencySource)
		}
		if experimentCase.DependencyMode != control.DependencyMVCCRuntime &&
			experimentCase.DependencySource != control.DependencySourceStaticProgram {
			return invalid("case %q mode %q requires static_program information", experimentCase.ID, experimentCase.DependencyMode)
		}
		if !control.ValidTraceMode(experimentCase.TraceMode) {
			return invalid("case %q has invalid trace_mode %q", experimentCase.ID, experimentCase.TraceMode)
		}
		if experimentCase.Engine != "serial" && experimentCase.Engine != "blockstm" {
			return invalid("case %q has unknown engine %q", experimentCase.ID, experimentCase.Engine)
		}
		if experimentCase.Policy != "serial_preset" && experimentCase.Policy != "blockstm_preset" {
			return invalid("case %q has unknown policy %q", experimentCase.ID, experimentCase.Policy)
		}
		if experimentCase.Engine == "serial" && experimentCase.MaxSpeculativeInflight > 1 {
			return invalid("serial case %q must use max_speculative_inflight 0 or 1", experimentCase.ID)
		}
		if experimentCase.Engine == "serial" &&
			(experimentCase.DependencyMode != control.DependencyMVCCRuntime ||
				experimentCase.DependencySource != control.DependencySourceRuntimeObserved) {
			return invalid("serial case %q must use mvcc_runtime/runtime_observed dependency control", experimentCase.ID)
		}
		caseIDs[experimentCase.ID] = experimentCase
	}
	if c.Output.ValidationBundle == "" || c.Output.ValidationRecords == "" || c.Output.RunRecords == "" || c.Output.ActionTraces == "" {
		return invalid("all output paths are required")
	}
	outputPaths := []string{c.Output.ValidationBundle, c.Output.ValidationRecords, c.Output.RunRecords, c.Output.ActionTraces}
	seenOutputPaths := make(map[string]struct{}, len(outputPaths))
	for _, path := range outputPaths {
		if _, exists := seenOutputPaths[path]; exists {
			return invalid("output paths must be distinct")
		}
		seenOutputPaths[path] = struct{}{}
	}
	if c.TelemetryAblation != nil {
		off, offExists := caseIDs[c.TelemetryAblation.OffCase]
		instrumented, instrumentedExists := caseIDs[c.TelemetryAblation.InstrumentedCase]
		if !offExists || !instrumentedExists {
			return invalid("telemetry ablation references an unknown case")
		}
		if off.TraceMode != control.TraceOff || instrumented.TraceMode == control.TraceOff {
			return invalid("telemetry ablation requires off and instrumented trace modes")
		}
		if off.Engine != instrumented.Engine || off.Policy != instrumented.Policy || off.Executors != instrumented.Executors ||
			off.MaxSpeculativeInflight != instrumented.MaxSpeculativeInflight ||
			off.DependencyMode != instrumented.DependencyMode || off.DependencySource != instrumented.DependencySource {
			return invalid("telemetry ablation cases may differ only by trace mode and id")
		}
		for _, platform := range c.TelemetryAblation.EnforcePlatforms {
			if platform != "linux" && platform != "darwin" {
				return invalid("unsupported telemetry ablation platform %q", platform)
			}
		}
	}
	return timeout, nil
}

func decodeStrict(encoded []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}
