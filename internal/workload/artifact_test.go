package workload_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/crypto-org-chain/go-block-stm/internal/workload"
	"github.com/crypto-org-chain/go-block-stm/internal/workload/synthetic"
)

func TestExecutionInputRequiresExplicitMetadataSourcesAndOmitsGroundTruth(t *testing.T) {
	artifact := generatedArtifact(t)
	artifact.EngineVisibleMetadata = []workload.MetadataRecord{
		workload.NewMetadataRecord(
			"metadata-1",
			artifact.OrderedBlocks[0].Transactions[0].ID,
			"rw_summary",
			workload.MetadataDeclared,
			"tx_admit",
			1,
			1,
			7,
			"runtime_validation_and_reexecute",
			[]byte("declared payload"),
		),
		workload.NewMetadataRecord(
			"metadata-2",
			artifact.OrderedBlocks[0].Transactions[0].ID,
			"rw_oracle",
			workload.MetadataOracleTestOnly,
			"validation_only",
			1,
			1,
			0,
			"reject_outside_oracle_mode",
			[]byte("oracle payload"),
		),
	}
	if err := artifact.Seal(); err != nil {
		t.Fatal(err)
	}

	baseline, err := artifact.ExecutionInput()
	if err != nil {
		t.Fatal(err)
	}
	if len(baseline.Metadata) != 0 {
		t.Fatalf("metadata was exposed without an allowed source: %#v", baseline.Metadata)
	}
	declared, err := artifact.ExecutionInput(workload.MetadataDeclared)
	if err != nil {
		t.Fatal(err)
	}
	if len(declared.Metadata) != 1 || declared.Metadata[0].Source != workload.MetadataDeclared {
		t.Fatalf("metadata source filter failed: %#v", declared.Metadata)
	}

	encoded, err := json.Marshal(declared)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "ground_truth") || strings.Contains(string(encoded), "oracle payload") {
		t.Fatalf("execution input leaked audit-only information: %s", encoded)
	}
	declared.Metadata[0].Payload[0] = 'X'
	if artifact.EngineVisibleMetadata[0].Payload[0] == 'X' {
		t.Fatal("execution input aliases artifact metadata")
	}
}

func TestDescriptorStrictRoundTrip(t *testing.T) {
	artifact := generatedArtifact(t)
	descriptor, err := artifact.Descriptor()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := workload.ParseDescriptor(descriptor)
	if err != nil {
		t.Fatal(err)
	}
	parsedDescriptor, err := parsed.Descriptor()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(descriptor, parsedDescriptor) {
		t.Fatalf("descriptor round trip changed bytes:\n%s\n%s", descriptor, parsedDescriptor)
	}

	withUnknownField := bytes.Replace(
		descriptor,
		[]byte(`{"schema_version"`),
		[]byte(`{"unknown_field":true,"schema_version"`),
		1,
	)
	if _, err := workload.ParseDescriptor(withUnknownField); !errors.Is(err, workload.ErrInvalidArtifact) {
		t.Fatalf("expected unknown field rejection, got %v", err)
	}
}

func TestArtifactDetectsContentAndMetadataTampering(t *testing.T) {
	artifact := generatedArtifact(t)
	copy := cloneArtifact(t, artifact)
	copy.InitialState[0].Value[0] ^= 0xff
	if err := copy.Validate(); !errors.Is(err, workload.ErrHashMismatch) {
		t.Fatalf("expected canonical hash mismatch, got %v", err)
	}

	artifact.EngineVisibleMetadata = []workload.MetadataRecord{
		workload.NewMetadataRecord(
			"metadata-1",
			artifact.OrderedBlocks[0].Transactions[0].ID,
			"prediction",
			workload.MetadataPredicted,
			"tx_admit",
			0.5,
			0.75,
			3,
			"runtime_validation_and_reexecute",
			[]byte("payload"),
		),
	}
	if err := artifact.Seal(); err != nil {
		t.Fatal(err)
	}
	copy = cloneArtifact(t, artifact)
	copy.EngineVisibleMetadata[0].Payload[0] ^= 0xff
	if err := copy.Validate(); !errors.Is(err, workload.ErrInvalidArtifact) {
		t.Fatalf("expected metadata integrity error, got %v", err)
	}
}

func TestArtifactRejectsUnknownMetadataVisibility(t *testing.T) {
	artifact := generatedArtifact(t)
	if _, err := artifact.ExecutionInput(workload.MetadataSource("unknown")); !errors.Is(err, workload.ErrInvalidArtifact) {
		t.Fatalf("expected invalid metadata source error, got %v", err)
	}
}

func TestArtifactRejectsInconsistentLogicalMetadata(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*workload.Artifact)
	}{
		{
			name: "missing operation id",
			mutate: func(artifact *workload.Artifact) {
				artifact.OrderedBlocks[0].Transactions[0].Program.Instructions[0].ID = ""
			},
		},
		{
			name: "ground truth key disagrees with instruction",
			mutate: func(artifact *workload.Artifact) {
				artifact.GroundTruth[0].Accesses[0].Key = []byte("wrong")
			},
		},
		{
			name: "arrival references wrong block",
			mutate: func(artifact *workload.Artifact) {
				artifact.LogicalArrivalSchedule[0].BlockID = "wrong"
			},
		},
		{
			name: "metadata target is unknown",
			mutate: func(artifact *workload.Artifact) {
				artifact.EngineVisibleMetadata = []workload.MetadataRecord{
					workload.NewMetadataRecord(
						"metadata-1",
						"unknown-target",
						"prediction",
						workload.MetadataPredicted,
						"tx_admit",
						0.5,
						0.5,
						1,
						"runtime_validation_and_reexecute",
						[]byte("payload"),
					),
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			artifact := generatedArtifact(t)
			test.mutate(&artifact)
			if err := artifact.Seal(); !errors.Is(err, workload.ErrInvalidArtifact) {
				t.Fatalf("expected invalid artifact error, got %v", err)
			}
		})
	}
}

func generatedArtifact(t *testing.T) workload.Artifact {
	t.Helper()
	artifact, err := synthetic.Generate(synthetic.Config{
		Seed:                 9,
		InitialKeys:          2,
		KeySpace:             2,
		BlockCount:           1,
		TransactionsPerBlock: 2,
		MaxComputeUnits:      2,
		TransactionMaxUnits:  6,
		FailureEvery:         0,
	})
	if err != nil {
		t.Fatal(err)
	}
	return artifact
}

func cloneArtifact(t *testing.T, artifact workload.Artifact) workload.Artifact {
	t.Helper()
	descriptor, err := artifact.Descriptor()
	if err != nil {
		t.Fatal(err)
	}
	var clone workload.Artifact
	if err := json.Unmarshal(descriptor, &clone); err != nil {
		t.Fatal(err)
	}
	return clone
}
