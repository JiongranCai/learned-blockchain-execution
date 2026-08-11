package experiment

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"reflect"
	"time"

	"github.com/crypto-org-chain/go-block-stm/internal/control"
	engineapi "github.com/crypto-org-chain/go-block-stm/internal/engine"
	"github.com/crypto-org-chain/go-block-stm/internal/engine/blockstm"
	"github.com/crypto-org-chain/go-block-stm/internal/engine/serial"
	"github.com/crypto-org-chain/go-block-stm/internal/model"
	"github.com/crypto-org-chain/go-block-stm/internal/policy"
	"github.com/crypto-org-chain/go-block-stm/internal/policy/fixed"
	"github.com/crypto-org-chain/go-block-stm/internal/telemetry"
	"github.com/crypto-org-chain/go-block-stm/internal/workload"
	"github.com/crypto-org-chain/go-block-stm/internal/workload/synthetic"
)

const aggregateResultSchema = "workload-result-v1"

var (
	ErrWorkloadHashMismatch = errors.New("workload hash does not match frozen config")
	ErrCanonicalMismatch    = errors.New("candidate result does not match serial oracle")
	ErrUnknownEngine        = errors.New("unknown engine")
	ErrUnknownPolicy        = errors.New("unknown policy")
)

type Execution struct {
	Results        []model.BlockResult
	Traces         []control.Trace
	Capabilities   control.Capabilities
	BlockLatencyNS []uint64
	ExecutionNS    uint64
	ResultDigest   string
	MaxRSSBytes    uint64
}

func LoadWorkload(config WorkloadConfig) (workload.Artifact, error) {
	var artifact workload.Artifact
	var err error
	if config.ArtifactPath != "" {
		encoded, readErr := os.ReadFile(config.ArtifactPath)
		if readErr != nil {
			return workload.Artifact{}, readErr
		}
		artifact, err = workload.ParseDescriptor(encoded)
	} else {
		artifact, err = synthetic.Generate(*config.Synthetic)
	}
	if err != nil {
		return workload.Artifact{}, err
	}
	if artifact.CanonicalHash != config.ExpectedHash {
		return workload.Artifact{}, fmt.Errorf("%w: got %s, want %s", ErrWorkloadHashMismatch, artifact.CanonicalHash, config.ExpectedHash)
	}
	return artifact, nil
}

func Execute(ctx context.Context, artifact workload.Artifact, experimentCase CaseConfig, omitDigest bool) (Execution, error) {
	input, err := artifact.ExecutionInput()
	if err != nil {
		return Execution{}, err
	}
	storage, err := input.NewState()
	if err != nil {
		return Execution{}, err
	}
	selectedEngine, selectedPolicy, err := resolveCase(experimentCase)
	if err != nil {
		return Execution{}, err
	}
	execution := Execution{
		Results:        make([]model.BlockResult, 0, len(input.OrderedBlocks)),
		Traces:         make([]control.Trace, 0, len(input.OrderedBlocks)),
		Capabilities:   selectedEngine.Capabilities(),
		BlockLatencyNS: make([]uint64, 0, len(input.OrderedBlocks)),
	}
	for _, block := range input.OrderedBlocks {
		started := time.Now()
		result, trace, executeErr := selectedEngine.ExecuteBlock(ctx, block, storage, engineapi.RunConfig{
			Executors:                       experimentCase.Executors,
			EpochID:                         input.ArtifactHash,
			Policy:                          selectedPolicy,
			TraceMode:                       experimentCase.TraceMode,
			MaxSpeculativeInflight:          experimentCase.MaxSpeculativeInflight,
			DependencyMode:                  experimentCase.DependencyMode,
			DependencySource:                experimentCase.DependencySource,
			DependencyRepresentation:        experimentCase.DependencyRepresentation,
			DependencyRepresentationBuilder: experimentCase.DependencyRepresentationBuilder,
			DependencyWaitPolicy:            experimentCase.DependencyWaitPolicy,
			DependencyEstimateInjection:     experimentCase.DependencyEstimateInjection,
			OmitResultDigest:                omitDigest,
		})
		elapsed := uint64(time.Since(started))
		execution.ExecutionNS += elapsed
		execution.BlockLatencyNS = append(execution.BlockLatencyNS, elapsed)
		if executeErr != nil {
			return Execution{}, executeErr
		}
		execution.Results = append(execution.Results, result)
		execution.Traces = append(execution.Traces, trace)
	}
	if omitDigest {
		for index := range execution.Results {
			execution.Results[index].Digest = model.CanonicalDigest(execution.Results[index])
		}
	}
	execution.ResultDigest = AggregateResultDigest(execution.Results)
	execution.MaxRSSBytes = telemetry.MaxRSSBytes()
	return execution, nil
}

func AggregateResultDigest(results []model.BlockResult) string {
	h := sha256.New()
	writeDigestField(h, []byte(aggregateResultSchema))
	var count [8]byte
	binary.BigEndian.PutUint64(count[:], uint64(len(results)))
	_, _ = h.Write(count[:])
	for _, result := range results {
		digest := result.Digest
		if digest == "" {
			digest = model.CanonicalDigest(result)
		}
		writeDigestField(h, []byte(digest))
	}
	return hex.EncodeToString(h.Sum(nil))
}

func ResultsEqual(left, right []model.BlockResult) bool {
	return reflect.DeepEqual(left, right)
}

func BlockDigests(results []model.BlockResult) []string {
	digests := make([]string, len(results))
	for index, result := range results {
		digests[index] = result.Digest
		if digests[index] == "" {
			digests[index] = model.CanonicalDigest(result)
		}
	}
	return digests
}

func resolveCase(experimentCase CaseConfig) (engineapi.Engine, policy.Policy, error) {
	var selectedEngine engineapi.Engine
	switch experimentCase.Engine {
	case "serial":
		selectedEngine = serial.New(nil)
	case "blockstm":
		selectedEngine = blockstm.New(nil)
	default:
		return nil, nil, fmt.Errorf("%w %q", ErrUnknownEngine, experimentCase.Engine)
	}
	var selectedPolicy policy.Policy
	switch experimentCase.Policy {
	case "serial_preset":
		selectedPolicy = fixed.NewSerialPreset()
	case "blockstm_preset":
		selectedPolicy = fixed.NewBlockSTMPreset()
	default:
		return nil, nil, fmt.Errorf("%w %q", ErrUnknownPolicy, experimentCase.Policy)
	}
	return selectedEngine, selectedPolicy, nil
}

func writeDigestField(writer interface{ Write([]byte) (int, error) }, value []byte) {
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(value)))
	_, _ = writer.Write(length[:])
	_, _ = writer.Write(value)
}
