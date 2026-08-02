package experiment

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"runtime"

	"github.com/crypto-org-chain/go-block-stm/internal/control"
	"github.com/crypto-org-chain/go-block-stm/internal/telemetry"
	"github.com/crypto-org-chain/go-block-stm/internal/workload"
)

const ValidationBundleSchemaVersion = "validation-bundle-v1"

var (
	ErrInvalidValidationBundle = errors.New("invalid validation bundle")
	ErrTelemetryBudget         = errors.New("telemetry overhead exceeds frozen budget")
)

type ValidatedCase struct {
	ID        string `json:"id"`
	Engine    string `json:"engine"`
	Policy    string `json:"policy"`
	Executors int    `json:"executors"`
}

type ValidationBundle struct {
	SchemaVersion            string          `json:"schema_version"`
	ConfigSchemaVersion      string          `json:"config_schema_version"`
	ConfigSchemaHash         string          `json:"config_schema_hash"`
	ConfigHash               string          `json:"config_hash"`
	StatisticalSchemaVersion string          `json:"statistical_schema_version"`
	StatisticalSchemaHash    string          `json:"statistical_schema_hash"`
	StatisticalProtocolHash  string          `json:"statistical_protocol_hash"`
	CodeCommit               string          `json:"code_commit"`
	CodeModified             bool            `json:"code_modified"`
	BinarySHA256             string          `json:"binary_sha256"`
	UpstreamCommit           string          `json:"upstream_commit"`
	WorkloadSchemaVersion    string          `json:"workload_schema_version"`
	WorkloadHash             string          `json:"workload_hash"`
	GeneratorVersion         string          `json:"generator_version"`
	GeneratorSeed            int64           `json:"generator_seed"`
	OracleEngine             string          `json:"oracle_engine"`
	OraclePolicy             string          `json:"oracle_policy"`
	BlockDigests             []string        `json:"block_digests"`
	ResultDigest             string          `json:"result_digest"`
	ValidatedCases           []ValidatedCase `json:"validated_cases"`
}

type WorkerRequest struct {
	CaseID string `json:"case_id"`
	Phase  string `json:"phase"`
	Round  int    `json:"round"`
	Order  int    `json:"order"`
}

type WorkerResponse struct {
	Record telemetry.BenchmarkRecord     `json:"record"`
	Traces []telemetry.ActionTraceRecord `json:"traces,omitempty"`
}

type WorkerInvoker func(context.Context, WorkerRequest) (WorkerResponse, error)

func Validate(ctx context.Context, loaded LoadedConfig) (ValidationBundle, error) {
	protocol, artifact, provenance, err := prepare(loaded)
	if err != nil {
		return ValidationBundle{}, err
	}
	oracleCase := CaseConfig{
		ID:        "serial-oracle",
		Engine:    "serial",
		Policy:    "serial_preset",
		Executors: 1,
		TraceMode: control.TraceOff,
	}
	oracleContext, cancel := context.WithTimeout(ctx, loaded.Timeout)
	oracle, err := Execute(oracleContext, artifact, oracleCase, false)
	cancel()
	if err != nil {
		return ValidationBundle{}, fmt.Errorf("serial oracle: %w", err)
	}
	records := make([]telemetry.ValidationRecord, 0, len(loaded.Config.Cases))
	validated := make([]ValidatedCase, 0, len(loaded.Config.Cases))
	var mismatch error
	for _, experimentCase := range loaded.Config.Cases {
		candidateContext, candidateCancel := context.WithTimeout(ctx, loaded.Timeout)
		candidate, executeErr := Execute(candidateContext, artifact, experimentCase, false)
		candidateCancel()
		if executeErr != nil {
			records = append(records, telemetry.ValidationRecord{
				SchemaVersion: telemetry.ValidationRecordSchema,
				RunID:         experimentCase.ID + "/validate",
				Status:        "failed",
				Error:         executeErr.Error(),
				Case:          experimentCase.TelemetryCase(),
				Capabilities:  candidate.Capabilities,
				Provenance:    provenance,
			})
			if writeErr := WriteJSONLines(loaded.Config.Output.ValidationRecords, validationValues(records)); writeErr != nil {
				return ValidationBundle{}, writeErr
			}
			return ValidationBundle{}, fmt.Errorf("validate case %s: %w", experimentCase.ID, executeErr)
		}
		match := ResultsEqual(oracle.Results, candidate.Results)
		records = append(records, telemetry.ValidationRecord{
			SchemaVersion:         telemetry.ValidationRecordSchema,
			RunID:                 experimentCase.ID + "/validate",
			Status:                "success",
			Case:                  experimentCase.TelemetryCase(),
			Capabilities:          candidate.Capabilities,
			Provenance:            provenance,
			OracleResultDigest:    oracle.ResultDigest,
			CandidateResultDigest: candidate.ResultDigest,
			CanonicalMatch:        match,
		})
		if !match && mismatch == nil {
			mismatch = fmt.Errorf("%w: case %s", ErrCanonicalMismatch, experimentCase.ID)
		}
		if match {
			validated = append(validated, ValidatedCase{
				ID:        experimentCase.ID,
				Engine:    experimentCase.Engine,
				Policy:    experimentCase.Policy,
				Executors: experimentCase.Executors,
			})
		}
	}
	if err := WriteJSONLines(loaded.Config.Output.ValidationRecords, validationValues(records)); err != nil {
		return ValidationBundle{}, err
	}
	if mismatch != nil {
		return ValidationBundle{}, mismatch
	}
	commit, modified := telemetry.BuildIdentity()
	binaryHash, err := telemetry.BinaryHash()
	if err != nil {
		return ValidationBundle{}, err
	}
	bundle := ValidationBundle{
		SchemaVersion:            ValidationBundleSchemaVersion,
		ConfigSchemaVersion:      ConfigSchemaVersion,
		ConfigSchemaHash:         SchemaHash(ConfigSchemaVersion),
		ConfigHash:               loaded.Hash,
		StatisticalSchemaVersion: StatisticalProtocolSchemaVersion,
		StatisticalSchemaHash:    SchemaHash(StatisticalProtocolSchemaVersion),
		StatisticalProtocolHash:  protocol.Hash,
		CodeCommit:               commit,
		CodeModified:             modified,
		BinarySHA256:             binaryHash,
		UpstreamCommit:           telemetry.UpstreamCommit,
		WorkloadSchemaVersion:    artifact.SchemaVersion,
		WorkloadHash:             artifact.CanonicalHash,
		GeneratorVersion:         artifact.Generator.Version,
		GeneratorSeed:            artifact.Generator.Seed,
		OracleEngine:             oracleCase.Engine,
		OraclePolicy:             oracleCase.Policy,
		BlockDigests:             BlockDigests(oracle.Results),
		ResultDigest:             oracle.ResultDigest,
		ValidatedCases:           validated,
	}
	if err := WriteJSON(loaded.Config.Output.ValidationBundle, bundle); err != nil {
		return ValidationBundle{}, err
	}
	return bundle, nil
}

func RunMatrix(ctx context.Context, loaded LoadedConfig, invoke WorkerInvoker) error {
	protocol, _, parentProvenance, err := prepare(loaded)
	if err != nil {
		return err
	}
	schedule := buildSchedule(loaded.Config)
	recordValues := make([]any, 0, len(schedule)+1)
	traceValues := make([]any, 0)
	samples := make(map[string][]uint64)
	for _, request := range schedule {
		workerContext, cancel := context.WithTimeout(ctx, loaded.Timeout)
		response, invokeErr := invoke(workerContext, request)
		timedOut := errors.Is(workerContext.Err(), context.DeadlineExceeded)
		cancel()
		if invokeErr == nil {
			invokeErr = validateWorkerResponse(response, request)
		}
		if invokeErr != nil {
			experimentCase, _ := lookupCase(loaded.Config.Cases, request.CaseID)
			capabilities := control.Capabilities{}
			if selectedEngine, _, resolveErr := resolveCase(experimentCase); resolveErr == nil {
				capabilities = selectedEngine.Capabilities()
			}
			status := "failed"
			if timedOut {
				status = "censored_timeout"
			}
			recordValues = append(recordValues, telemetry.BenchmarkRecord{
				SchemaVersion: telemetry.BenchmarkRecordSchema,
				RunID:         runID(request),
				Mode:          "performance",
				Status:        status,
				Censored:      timedOut,
				Error:         invokeErr.Error(),
				Phase:         request.Phase,
				Round:         request.Round,
				Order:         request.Order,
				Case:          experimentCase.TelemetryCase(),
				Capabilities:  capabilities,
				Provenance:    parentProvenance,
			})
			if writeErr := writeRunOutputs(loaded, recordValues, traceValues); writeErr != nil {
				return writeErr
			}
			return fmt.Errorf("run %s/%s/%d: %w", request.CaseID, request.Phase, request.Round, invokeErr)
		}
		recordValues = append(recordValues, response.Record)
		if response.Record.Phase == "measurement" {
			samples[response.Record.Case.ID] = append(samples[response.Record.Case.ID], response.Record.Timing.ExecutionNS)
		}
		for _, trace := range response.Traces {
			traceValues = append(traceValues, trace)
		}
	}
	if ablation := loaded.Config.TelemetryAblation; ablation != nil {
		enforced := containsString(ablation.EnforcePlatforms, runtime.GOOS)
		record, recordErr := telemetry.NewAblationRecord(
			ablation.OffCase,
			ablation.InstrumentedCase,
			samples[ablation.OffCase],
			samples[ablation.InstrumentedCase],
			protocol.Protocol.TelemetryOverheadBudgetRatio,
			enforced,
		)
		if recordErr != nil {
			if writeErr := writeRunOutputs(loaded, recordValues, traceValues); writeErr != nil {
				return writeErr
			}
			return recordErr
		}
		recordValues = append(recordValues, record)
		if enforced && !record.WithinBudget {
			if err := writeRunOutputs(loaded, recordValues, traceValues); err != nil {
				return err
			}
			return fmt.Errorf("%w: observed %.4f, budget %.4f", ErrTelemetryBudget, record.OverheadRatio, record.BudgetRatio)
		}
	}
	if err := WriteJSONLines(loaded.Config.Output.RunRecords, recordValues); err != nil {
		return err
	}
	return WriteJSONLines(loaded.Config.Output.ActionTraces, traceValues)
}

func RunWorker(ctx context.Context, loaded LoadedConfig, request WorkerRequest) (WorkerResponse, error) {
	protocol, artifact, provenance, err := prepare(loaded)
	if err != nil {
		return WorkerResponse{}, err
	}
	bundle, err := LoadValidationBundle(loaded.Config.Output.ValidationBundle)
	if err != nil {
		return WorkerResponse{}, err
	}
	if err := validateBundle(bundle, loaded, protocol, artifact); err != nil {
		return WorkerResponse{}, err
	}
	experimentCase, ok := lookupCase(loaded.Config.Cases, request.CaseID)
	if !ok {
		return WorkerResponse{}, fmt.Errorf("%w: unknown case %q", ErrInvalidConfig, request.CaseID)
	}
	if !bundleHasCase(bundle, experimentCase) {
		return WorkerResponse{}, fmt.Errorf("%w: case %q was not validated", ErrInvalidValidationBundle, request.CaseID)
	}
	execution, err := Execute(ctx, artifact, experimentCase, true)
	if err != nil {
		return WorkerResponse{}, err
	}
	match := execution.ResultDigest == bundle.ResultDigest && stringSlicesEqual(BlockDigests(execution.Results), bundle.BlockDigests)
	runID := runID(request)
	record := telemetry.BenchmarkRecord{
		SchemaVersion: telemetry.BenchmarkRecordSchema,
		RunID:         runID,
		Mode:          "performance",
		Status:        "success",
		Phase:         request.Phase,
		Round:         request.Round,
		Order:         request.Order,
		Case:          experimentCase.TelemetryCase(),
		Capabilities:  execution.Capabilities,
		Provenance:    provenance,
		Timing: telemetry.Timing{
			ExecutionNS:    execution.ExecutionNS,
			BlockLatencyNS: execution.BlockLatencyNS,
		},
		Metrics:        telemetry.CollectMetrics(execution.Results, execution.Traces, execution.ExecutionNS, execution.MaxRSSBytes),
		BlockDigests:   BlockDigests(execution.Results),
		ResultDigest:   execution.ResultDigest,
		CanonicalMatch: match,
	}
	if !match {
		return WorkerResponse{}, fmt.Errorf("%w: performance run %s", ErrCanonicalMismatch, runID)
	}
	response := WorkerResponse{Record: record}
	if experimentCase.TraceMode == control.TraceDetailed {
		response.Traces = make([]telemetry.ActionTraceRecord, 0, len(execution.Traces))
		for index, trace := range execution.Traces {
			blockID := execution.Results[index].BlockID
			response.Traces = append(response.Traces, telemetry.ActionTraceRecord{
				SchemaVersion: telemetry.ActionTraceSchema,
				RunID:         runID,
				BlockID:       blockID,
				Trace:         trace,
			})
		}
	}
	return response, nil
}

func prepare(loaded LoadedConfig) (LoadedProtocol, workload.Artifact, telemetry.Provenance, error) {
	protocol, err := LoadStatisticalProtocol(loaded.Config.StatisticalProtocol)
	if err != nil {
		return LoadedProtocol{}, workload.Artifact{}, telemetry.Provenance{}, err
	}
	if err := protocol.Protocol.ValidateRunClass(loaded.Config); err != nil {
		return LoadedProtocol{}, workload.Artifact{}, telemetry.Provenance{}, err
	}
	artifact, err := LoadWorkload(loaded.Config.Workload)
	if err != nil {
		return LoadedProtocol{}, workload.Artifact{}, telemetry.Provenance{}, err
	}
	commit, modified := telemetry.BuildIdentity()
	binaryHash, err := telemetry.BinaryHash()
	if err != nil {
		return LoadedProtocol{}, workload.Artifact{}, telemetry.Provenance{}, err
	}
	if loaded.Config.RunClass == "formal" && (commit == "unknown" || modified) {
		return LoadedProtocol{}, workload.Artifact{}, telemetry.Provenance{}, fmt.Errorf("%w: formal runs require a clean VCS-stamped binary", ErrInvalidConfig)
	}
	provenance := telemetry.Provenance{
		CodeCommit:               commit,
		CodeModified:             modified,
		BinarySHA256:             binaryHash,
		ProcessID:                os.Getpid(),
		UpstreamCommit:           telemetry.UpstreamCommit,
		ConfigSchemaVersion:      ConfigSchemaVersion,
		ConfigSchemaHash:         SchemaHash(ConfigSchemaVersion),
		ConfigHash:               loaded.Hash,
		StatisticalSchemaVersion: StatisticalProtocolSchemaVersion,
		StatisticalSchemaHash:    SchemaHash(StatisticalProtocolSchemaVersion),
		StatisticalProtocolHash:  protocol.Hash,
		WorkloadSchemaVersion:    artifact.SchemaVersion,
		WorkloadHash:             artifact.CanonicalHash,
		GeneratorVersion:         artifact.Generator.Version,
		GeneratorSeed:            artifact.Generator.Seed,
		Hardware:                 telemetry.CollectHardware(),
		Environment:              loaded.Config.Environment,
		RunProtocol: telemetry.RunProtocol{
			RunClass:          loaded.Config.RunClass,
			WarmupRounds:      loaded.Config.WarmupRounds,
			MeasurementRounds: loaded.Config.MeasurementRounds,
			OrderSeed:         loaded.Config.OrderSeed,
			Timeout:           loaded.Config.Timeout,
		},
	}
	return protocol, artifact, provenance, nil
}

func LoadValidationBundle(path string) (ValidationBundle, error) {
	var bundle ValidationBundle
	if err := ReadJSON(path, &bundle); err != nil {
		return ValidationBundle{}, err
	}
	if bundle.SchemaVersion != ValidationBundleSchemaVersion || bundle.ResultDigest == "" || len(bundle.BlockDigests) == 0 {
		return ValidationBundle{}, fmt.Errorf("%w: incomplete bundle", ErrInvalidValidationBundle)
	}
	return bundle, nil
}

func validateBundle(bundle ValidationBundle, loaded LoadedConfig, protocol LoadedProtocol, artifact workload.Artifact) error {
	commit, modified := telemetry.BuildIdentity()
	binaryHash, err := telemetry.BinaryHash()
	if err != nil {
		return err
	}
	if bundle.ConfigSchemaVersion != ConfigSchemaVersion || bundle.ConfigSchemaHash != SchemaHash(ConfigSchemaVersion) || bundle.ConfigHash != loaded.Hash ||
		bundle.StatisticalSchemaVersion != StatisticalProtocolSchemaVersion || bundle.StatisticalSchemaHash != SchemaHash(StatisticalProtocolSchemaVersion) || bundle.StatisticalProtocolHash != protocol.Hash ||
		bundle.CodeCommit != commit || bundle.CodeModified != modified || bundle.BinarySHA256 != binaryHash || bundle.UpstreamCommit != telemetry.UpstreamCommit ||
		bundle.WorkloadSchemaVersion != artifact.SchemaVersion || bundle.WorkloadHash != artifact.CanonicalHash ||
		bundle.GeneratorVersion != artifact.Generator.Version || bundle.GeneratorSeed != artifact.Generator.Seed {
		return fmt.Errorf("%w: provenance does not match current binary/config/workload", ErrInvalidValidationBundle)
	}
	return nil
}

func buildSchedule(config Config) []WorkerRequest {
	schedule := make([]WorkerRequest, 0, (config.WarmupRounds+config.MeasurementRounds)*len(config.Cases))
	appendRounds := func(phase string, rounds int, seedOffset int64) {
		for round := 0; round < rounds; round++ {
			indices := make([]int, len(config.Cases))
			for index := range indices {
				indices[index] = index
			}
			rng := rand.New(rand.NewSource(config.OrderSeed + seedOffset + int64(round)))
			rng.Shuffle(len(indices), func(i, j int) { indices[i], indices[j] = indices[j], indices[i] })
			for order, index := range indices {
				schedule = append(schedule, WorkerRequest{
					CaseID: config.Cases[index].ID,
					Phase:  phase,
					Round:  round,
					Order:  order,
				})
			}
		}
	}
	appendRounds("warmup", config.WarmupRounds, 0)
	appendRounds("measurement", config.MeasurementRounds, 1<<32)
	return schedule
}

func bundleHasCase(bundle ValidationBundle, experimentCase CaseConfig) bool {
	for _, candidate := range bundle.ValidatedCases {
		if candidate.ID == experimentCase.ID && candidate.Engine == experimentCase.Engine &&
			candidate.Policy == experimentCase.Policy && candidate.Executors == experimentCase.Executors {
			return true
		}
	}
	return false
}

func lookupCase(cases []CaseConfig, id string) (CaseConfig, bool) {
	for _, experimentCase := range cases {
		if experimentCase.ID == id {
			return experimentCase, true
		}
	}
	return CaseConfig{}, false
}

func validationValues(records []telemetry.ValidationRecord) []any {
	values := make([]any, len(records))
	for index := range records {
		values[index] = records[index]
	}
	return values
}

func stringSlicesEqual(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func runID(request WorkerRequest) string {
	return fmt.Sprintf("%s/%s/%04d/%02d", request.CaseID, request.Phase, request.Round, request.Order)
}

func writeRunOutputs(loaded LoadedConfig, records, traces []any) error {
	if err := WriteJSONLines(loaded.Config.Output.RunRecords, records); err != nil {
		return err
	}
	return WriteJSONLines(loaded.Config.Output.ActionTraces, traces)
}

func validateWorkerResponse(response WorkerResponse, request WorkerRequest) error {
	record := response.Record
	if record.SchemaVersion != telemetry.BenchmarkRecordSchema || record.RunID != runID(request) ||
		record.Status != "success" || record.Censored || !record.CanonicalMatch ||
		record.Case.ID != request.CaseID || record.Phase != request.Phase || record.Round != request.Round || record.Order != request.Order {
		return errors.New("worker returned an invalid benchmark record")
	}
	for _, trace := range response.Traces {
		if trace.SchemaVersion != telemetry.ActionTraceSchema || trace.RunID != record.RunID {
			return errors.New("worker returned an invalid action trace record")
		}
	}
	return nil
}
