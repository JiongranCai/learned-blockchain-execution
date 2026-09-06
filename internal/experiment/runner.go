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

var ErrTelemetryBudget = errors.New("telemetry overhead exceeds frozen budget")

type ValidationResult struct {
	Cases        int
	ResultDigest string
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

func Validate(ctx context.Context, loaded LoadedConfig) (ValidationResult, error) {
	_, artifact, provenance, err := prepare(loaded)
	if err != nil {
		return ValidationResult{}, err
	}
	return validate(ctx, loaded, artifact, provenance)
}

func validate(ctx context.Context, loaded LoadedConfig, artifact workload.Artifact, provenance telemetry.Provenance) (ValidationResult, error) {
	oracleCase := CaseConfig{
		ID:                              "serial-oracle",
		Engine:                          "serial",
		Policy:                          "serial_preset",
		Executors:                       1,
		DependencyMode:                  control.DependencyMVCCRuntime,
		DependencySource:                control.DependencySourceRuntimeObserved,
		DependencyRepresentation:        control.DependencyRepresentationVersionOnly,
		DependencyRepresentationBuilder: control.DependencyRepresentationBuilderNone,
		DependencyWaitPolicy:            control.DependencyWaitNone,
		DependencyEstimateInjection:     control.DependencyEstimatesDisabled,
		DependencyDispatch:              control.DependencyDispatchIndexOrder,
		EstimateReadPolicy:              control.EstimateReadSuspendInPlace,
		IdleWaitPolicy:                  control.IdleWaitGosched,
		TraceMode:                       control.TraceOff,
	}
	oracleContext, cancel := context.WithTimeout(ctx, loaded.Timeout)
	oracle, err := Execute(oracleContext, artifact, oracleCase, false)
	cancel()
	if err != nil {
		return ValidationResult{}, fmt.Errorf("serial oracle: %w", err)
	}
	records := make([]any, 0, len(loaded.Config.Cases))
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
			if writeErr := WriteJSONLines(loaded.Config.Output.ValidationRecords, records); writeErr != nil {
				return ValidationResult{}, writeErr
			}
			return ValidationResult{}, fmt.Errorf("validate case %s: %w", experimentCase.ID, executeErr)
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
	}
	if err := WriteJSONLines(loaded.Config.Output.ValidationRecords, records); err != nil {
		return ValidationResult{}, err
	}
	if mismatch != nil {
		return ValidationResult{}, mismatch
	}
	return ValidationResult{Cases: len(records), ResultDigest: oracle.ResultDigest}, nil
}

func RunMatrix(ctx context.Context, loaded LoadedConfig, invoke WorkerInvoker) error {
	protocol, artifact, parentProvenance, err := prepare(loaded)
	if err != nil {
		return err
	}
	oracle, err := validate(ctx, loaded, artifact, parentProvenance)
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
			response.Record.CanonicalMatch = response.Record.ResultDigest == oracle.ResultDigest
			if !response.Record.CanonicalMatch {
				invokeErr = ErrCanonicalMismatch
			}
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
			protocol.TelemetryOverheadBudgetRatio,
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
	_, artifact, provenance, err := prepare(loaded)
	if err != nil {
		return WorkerResponse{}, err
	}
	experimentCase, ok := lookupCase(loaded.Config.Cases, request.CaseID)
	if !ok {
		return WorkerResponse{}, fmt.Errorf("%w: unknown case %q", ErrInvalidConfig, request.CaseID)
	}
	execution, err := Execute(ctx, artifact, experimentCase, true)
	if err != nil {
		return WorkerResponse{}, err
	}
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
		Metrics:      telemetry.CollectMetrics(execution.Results, execution.Traces, execution.ExecutionNS, execution.MaxRSSBytes),
		BlockDigests: BlockDigests(execution.Results),
		ResultDigest: execution.ResultDigest,
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

func prepare(loaded LoadedConfig) (StatisticalProtocol, workload.Artifact, telemetry.Provenance, error) {
	protocol, err := LoadStatisticalProtocol(loaded.Config.StatisticalProtocol)
	if err != nil {
		return StatisticalProtocol{}, workload.Artifact{}, telemetry.Provenance{}, err
	}
	if err := protocol.ValidateRunClass(loaded.Config); err != nil {
		return StatisticalProtocol{}, workload.Artifact{}, telemetry.Provenance{}, err
	}
	artifact, err := LoadWorkload(loaded.Config.Workload)
	if err != nil {
		return StatisticalProtocol{}, workload.Artifact{}, telemetry.Provenance{}, err
	}
	commit, modified := telemetry.BuildIdentity()
	provenance := telemetry.Provenance{
		CodeCommit:               commit,
		CodeModified:             modified,
		ProcessID:                os.Getpid(),
		UpstreamCommit:           telemetry.UpstreamCommit,
		ConfigPath:               loaded.Path,
		ConfigSchemaVersion:      ConfigSchemaVersion,
		StatisticalSchemaVersion: StatisticalProtocolSchemaVersion,
		WorkloadSchemaVersion:    artifact.SchemaVersion,
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

func lookupCase(cases []CaseConfig, id string) (CaseConfig, bool) {
	for _, experimentCase := range cases {
		if experimentCase.ID == id {
			return experimentCase, true
		}
	}
	return CaseConfig{}, false
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
