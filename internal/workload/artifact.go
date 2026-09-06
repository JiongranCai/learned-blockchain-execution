package workload

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"

	"github.com/crypto-org-chain/go-block-stm/internal/model"
	"github.com/crypto-org-chain/go-block-stm/internal/state/memkv"
)

const ArtifactSchemaVersion = "workload-artifact-v3"

var (
	ErrInvalidArtifact = errors.New("invalid workload artifact")
)

type GeneratorDescriptor struct {
	Name    string          `json:"name"`
	Version string          `json:"version"`
	Seed    int64           `json:"seed"`
	Config  json.RawMessage `json:"config"`
}

// LogicalArrival is workload timing input, not transaction semantics.
// Sequence is the deterministic tie-break when logical times are equal.
type LogicalArrival struct {
	Sequence      uint64 `json:"sequence"`
	LogicalTime   uint64 `json:"logical_time"`
	BlockID       string `json:"block_id"`
	TransactionID string `json:"transaction_id"`
}

type MetadataSource string

const (
	MetadataDeclared        MetadataSource = "declared"
	MetadataObservedHistory MetadataSource = "observed_history"
	MetadataPredicted       MetadataSource = "predicted"
	MetadataOracleTestOnly  MetadataSource = "oracle_test_only"
)

// MetadataRecord is the only workload metadata eligible for explicit engine
// exposure, with its source and acquisition semantics.
type MetadataRecord struct {
	ID              string         `json:"id"`
	TargetID        string         `json:"target_id"`
	Kind            string         `json:"kind"`
	Source          MetadataSource `json:"source"`
	AvailableAt     string         `json:"available_at"`
	Completeness    float64        `json:"completeness"`
	Confidence      float64        `json:"confidence"`
	AcquisitionCost uint64         `json:"acquisition_cost"`
	MissSemantics   string         `json:"miss_semantics"`
	Payload         []byte         `json:"payload"`
}

func NewMetadataRecord(
	id string,
	targetID string,
	kind string,
	source MetadataSource,
	availableAt string,
	completeness float64,
	confidence float64,
	acquisitionCost uint64,
	missSemantics string,
	payload []byte,
) MetadataRecord {
	return MetadataRecord{
		ID:              id,
		TargetID:        targetID,
		Kind:            kind,
		Source:          source,
		AvailableAt:     availableAt,
		Completeness:    completeness,
		Confidence:      confidence,
		AcquisitionCost: acquisitionCost,
		MissSemantics:   missSemantics,
		Payload:         cloneBytes(payload),
	}
}

// Artifact contains one generated workload and its optional input metadata.
type Artifact struct {
	SchemaVersion          string              `json:"schema_version"`
	Generator              GeneratorDescriptor `json:"generator"`
	InitialState           []model.StateEntry  `json:"initial_state"`
	OrderedBlocks          []model.Block       `json:"ordered_blocks"`
	LogicalArrivalSchedule []LogicalArrival    `json:"logical_arrival_schedule"`
	EngineVisibleMetadata  []MetadataRecord    `json:"engine_visible_metadata"`
}

// ExecutionInput contains the workload and explicitly selected input metadata.
type ExecutionInput struct {
	InitialState           []model.StateEntry `json:"initial_state"`
	OrderedBlocks          []model.Block      `json:"ordered_blocks"`
	LogicalArrivalSchedule []LogicalArrival   `json:"logical_arrival_schedule"`
	Metadata               []MetadataRecord   `json:"metadata"`
}

func (a Artifact) Descriptor() ([]byte, error) {
	if err := a.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(a)
}

func ParseDescriptor(encoded []byte) (Artifact, error) {
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var artifact Artifact
	if err := decoder.Decode(&artifact); err != nil {
		return Artifact{}, invalidArtifact("decode descriptor: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return Artifact{}, invalidArtifact("descriptor contains multiple JSON values")
		}
		return Artifact{}, invalidArtifact("decode descriptor trailer: %v", err)
	}
	if err := artifact.Validate(); err != nil {
		return Artifact{}, err
	}
	return artifact, nil
}

func (a Artifact) NewState() (*memkv.Store, error) {
	if err := a.Validate(); err != nil {
		return nil, err
	}
	return memkv.FromEntries(a.InitialState)
}

// ExecutionInput returns a deep-cloned engine view filtered to explicitly
// allowed metadata sources. Passing no sources exposes no metadata.
func (a Artifact) ExecutionInput(allowedSources ...MetadataSource) (ExecutionInput, error) {
	allowed := make(map[MetadataSource]struct{}, len(allowedSources))
	for _, source := range allowedSources {
		if !validMetadataSource(source) {
			return ExecutionInput{}, invalidArtifact("unknown allowed metadata source %q", source)
		}
		allowed[source] = struct{}{}
	}
	metadata := make([]MetadataRecord, 0, len(a.EngineVisibleMetadata))
	for _, record := range a.EngineVisibleMetadata {
		if _, ok := allowed[record.Source]; ok {
			metadata = append(metadata, record)
		}
	}

	input := ExecutionInput{
		InitialState:           a.InitialState,
		OrderedBlocks:          a.OrderedBlocks,
		LogicalArrivalSchedule: a.LogicalArrivalSchedule,
		Metadata:               metadata,
	}
	encoded, err := json.Marshal(input)
	if err != nil {
		return ExecutionInput{}, err
	}
	var clone ExecutionInput
	err = json.Unmarshal(encoded, &clone)
	return clone, err
}

func (input ExecutionInput) NewState() (*memkv.Store, error) {
	return memkv.FromEntries(input.InitialState)
}

func (a Artifact) Validate() error {
	if a.SchemaVersion != ArtifactSchemaVersion {
		return invalidArtifact("schema_version must be %q", ArtifactSchemaVersion)
	}
	if a.Generator.Name == "" || a.Generator.Version == "" {
		return invalidArtifact("generator name and version are required")
	}
	if len(a.Generator.Config) == 0 || !json.Valid(a.Generator.Config) {
		return invalidArtifact("generator config must be valid JSON")
	}
	for index := 1; index < len(a.InitialState); index++ {
		if bytes.Compare(a.InitialState[index-1].Key, a.InitialState[index].Key) >= 0 {
			return invalidArtifact("initial_state keys must be strictly byte-sorted")
		}
	}

	logicalIDs := make(map[string]struct{})
	txIDs := make(map[string]string)
	for _, block := range a.OrderedBlocks {
		if block.ID == "" {
			return invalidArtifact("block id is required")
		}
		if _, exists := logicalIDs[block.ID]; exists {
			return invalidArtifact("duplicate logical id %q", block.ID)
		}
		logicalIDs[block.ID] = struct{}{}
		for _, transaction := range block.Transactions {
			if transaction.ID == "" {
				return invalidArtifact("transaction id is required")
			}
			txIDs[transaction.ID] = block.ID
			if _, exists := logicalIDs[transaction.ID]; exists {
				return invalidArtifact("duplicate logical id %q", transaction.ID)
			}
			logicalIDs[transaction.ID] = struct{}{}
			for _, instruction := range transaction.Program.Instructions {
				if instruction.ID == "" {
					return invalidArtifact("transaction %q has an operation without id", transaction.ID)
				}
				if _, exists := logicalIDs[instruction.ID]; exists {
					return invalidArtifact("duplicate logical id %q", instruction.ID)
				}
				logicalIDs[instruction.ID] = struct{}{}
			}
		}
	}

	if len(a.LogicalArrivalSchedule) != len(txIDs) {
		return invalidArtifact("logical arrival count does not match transaction count")
	}
	arrivals := make(map[string]struct{}, len(a.LogicalArrivalSchedule))
	for index, arrival := range a.LogicalArrivalSchedule {
		if arrival.Sequence != uint64(index) {
			return invalidArtifact("logical arrival sequence must be contiguous")
		}
		if index > 0 && a.LogicalArrivalSchedule[index-1].LogicalTime > arrival.LogicalTime {
			return invalidArtifact("logical arrival time must be nondecreasing")
		}
		blockID, exists := txIDs[arrival.TransactionID]
		if !exists || blockID != arrival.BlockID {
			return invalidArtifact("logical arrival references unknown block/transaction")
		}
		if _, exists := arrivals[arrival.TransactionID]; exists {
			return invalidArtifact("duplicate logical arrival for %q", arrival.TransactionID)
		}
		arrivals[arrival.TransactionID] = struct{}{}
	}

	metadataIDs := make(map[string]struct{}, len(a.EngineVisibleMetadata))
	for _, record := range a.EngineVisibleMetadata {
		if record.ID == "" || record.TargetID == "" || record.Kind == "" || record.AvailableAt == "" {
			return invalidArtifact("metadata identity, target, kind, and available_at are required")
		}
		if _, exists := metadataIDs[record.ID]; exists {
			return invalidArtifact("duplicate metadata id %q", record.ID)
		}
		metadataIDs[record.ID] = struct{}{}
		if _, exists := logicalIDs[record.TargetID]; !exists {
			return invalidArtifact("metadata %q references unknown target %q", record.ID, record.TargetID)
		}
		if !validMetadataSource(record.Source) {
			return invalidArtifact("metadata %q has invalid source", record.ID)
		}
		if !probability(record.Completeness) || !probability(record.Confidence) {
			return invalidArtifact("metadata %q completeness/confidence must be in [0,1]", record.ID)
		}
		if record.MissSemantics == "" {
			return invalidArtifact("metadata %q miss_semantics is required", record.ID)
		}
	}
	return nil
}

func validMetadataSource(source MetadataSource) bool {
	switch source {
	case MetadataDeclared, MetadataObservedHistory, MetadataPredicted, MetadataOracleTestOnly:
		return true
	default:
		return false
	}
}

func probability(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0 && value <= 1
}

func invalidArtifact(format string, arguments ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidArtifact, fmt.Sprintf(format, arguments...))
}

func cloneBytes(value []byte) []byte {
	if value == nil {
		return nil
	}
	return append([]byte(nil), value...)
}
