package workload

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"

	"github.com/crypto-org-chain/go-block-stm/internal/model"
	"github.com/crypto-org-chain/go-block-stm/internal/state/memkv"
)

const ArtifactSchemaVersion = "workload-artifact-v1"

var (
	ErrInvalidArtifact = errors.New("invalid workload artifact")
	ErrHashMismatch    = errors.New("workload artifact canonical hash mismatch")
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

type AccessMode string

const (
	AccessRead   AccessMode = "read"
	AccessWrite  AccessMode = "write"
	AccessDelete AccessMode = "delete"
)

type GroundTruthAccess struct {
	OperationID string     `json:"operation_id"`
	Mode        AccessMode `json:"mode"`
	Key         []byte     `json:"key"`
}

type BranchOutcome struct {
	BranchID string `json:"branch_id"`
	Taken    bool   `json:"taken"`
	Target   int    `json:"target"`
}

// TransactionGroundTruth is audit-only information. It is deliberately absent
// from ExecutionInput so a candidate engine cannot receive generator knowledge
// merely because it consumes the artifact.
type TransactionGroundTruth struct {
	TransactionID  string              `json:"transaction_id"`
	ExpectedStatus model.TxStatus      `json:"expected_status"`
	OperationPath  []string            `json:"operation_path"`
	Accesses       []GroundTruthAccess `json:"accesses"`
	Branches       []BranchOutcome     `json:"branches"`
}

type MetadataSource string

const (
	MetadataDeclared        MetadataSource = "declared"
	MetadataObservedHistory MetadataSource = "observed_history"
	MetadataPredicted       MetadataSource = "predicted"
	MetadataOracleTestOnly  MetadataSource = "oracle_test_only"
)

// MetadataRecord is the only workload metadata eligible for explicit engine
// exposure. Payload integrity and acquisition semantics travel with the data.
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
	Size            uint64         `json:"size"`
	Hash            string         `json:"hash"`
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
	digest := sha256.Sum256(payload)
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
		Size:            uint64(len(payload)),
		Hash:            hex.EncodeToString(digest[:]),
	}
}

// Artifact is the complete, immutable input/audit record for one generated
// workload. CanonicalHash authenticates every field except itself.
type Artifact struct {
	SchemaVersion          string                   `json:"schema_version"`
	Generator              GeneratorDescriptor      `json:"generator"`
	InitialState           []model.StateEntry       `json:"initial_state"`
	OrderedBlocks          []model.Block            `json:"ordered_blocks"`
	LogicalArrivalSchedule []LogicalArrival         `json:"logical_arrival_schedule"`
	GroundTruth            []TransactionGroundTruth `json:"ground_truth"`
	EngineVisibleMetadata  []MetadataRecord         `json:"engine_visible_metadata"`
	CanonicalHash          string                   `json:"canonical_hash"`
}

// ExecutionInput contains only data available to an execution run. In
// particular, generator ground truth is not representable in this type.
type ExecutionInput struct {
	ArtifactHash           string             `json:"artifact_hash"`
	InitialState           []model.StateEntry `json:"initial_state"`
	OrderedBlocks          []model.Block      `json:"ordered_blocks"`
	LogicalArrivalSchedule []LogicalArrival   `json:"logical_arrival_schedule"`
	Metadata               []MetadataRecord   `json:"metadata"`
}

// Seal validates the artifact body and installs its canonical hash.
func (a *Artifact) Seal() error {
	if a == nil {
		return invalidArtifact("artifact is nil")
	}
	if err := a.validateBody(); err != nil {
		return err
	}
	digest, err := a.computeCanonicalHash()
	if err != nil {
		return err
	}
	a.CanonicalHash = digest
	return nil
}

func (a Artifact) Validate() error {
	if err := a.validateBody(); err != nil {
		return err
	}
	if a.CanonicalHash == "" {
		return invalidArtifact("canonical_hash is required")
	}
	want, err := a.computeCanonicalHash()
	if err != nil {
		return err
	}
	if a.CanonicalHash != want {
		return fmt.Errorf("%w: got %s, want %s", ErrHashMismatch, a.CanonicalHash, want)
	}
	return nil
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

func (a Artifact) DescriptorDigest() (string, error) {
	if err := a.Validate(); err != nil {
		return "", err
	}
	return a.CanonicalHash, nil
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
	descriptor, err := a.Descriptor()
	if err != nil {
		return ExecutionInput{}, err
	}
	clone, err := ParseDescriptor(descriptor)
	if err != nil {
		return ExecutionInput{}, err
	}

	allowed := make(map[MetadataSource]struct{}, len(allowedSources))
	for _, source := range allowedSources {
		if !validMetadataSource(source) {
			return ExecutionInput{}, invalidArtifact("unknown allowed metadata source %q", source)
		}
		allowed[source] = struct{}{}
	}
	metadata := make([]MetadataRecord, 0, len(clone.EngineVisibleMetadata))
	for _, record := range clone.EngineVisibleMetadata {
		if _, ok := allowed[record.Source]; ok {
			metadata = append(metadata, record)
		}
	}

	return ExecutionInput{
		ArtifactHash:           clone.CanonicalHash,
		InitialState:           clone.InitialState,
		OrderedBlocks:          clone.OrderedBlocks,
		LogicalArrivalSchedule: clone.LogicalArrivalSchedule,
		Metadata:               metadata,
	}, nil
}

func (input ExecutionInput) NewState() (*memkv.Store, error) {
	return memkv.FromEntries(input.InitialState)
}

func (a Artifact) computeCanonicalHash() (string, error) {
	payload := struct {
		SchemaVersion          string                   `json:"schema_version"`
		Generator              GeneratorDescriptor      `json:"generator"`
		InitialState           []model.StateEntry       `json:"initial_state"`
		OrderedBlocks          []model.Block            `json:"ordered_blocks"`
		LogicalArrivalSchedule []LogicalArrival         `json:"logical_arrival_schedule"`
		GroundTruth            []TransactionGroundTruth `json:"ground_truth"`
		EngineVisibleMetadata  []MetadataRecord         `json:"engine_visible_metadata"`
	}{
		SchemaVersion:          a.SchemaVersion,
		Generator:              a.Generator,
		InitialState:           a.InitialState,
		OrderedBlocks:          a.OrderedBlocks,
		LogicalArrivalSchedule: a.LogicalArrivalSchedule,
		GroundTruth:            a.GroundTruth,
		EngineVisibleMetadata:  a.EngineVisibleMetadata,
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	_, _ = h.Write([]byte(ArtifactSchemaVersion))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write(encoded)
	return hex.EncodeToString(h.Sum(nil)), nil
}

func (a Artifact) validateBody() error {
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

	blockIDs := make(map[string]struct{}, len(a.OrderedBlocks))
	logicalIDs := make(map[string]struct{})
	txIDs := make(map[string]string)
	txOrder := make([]string, 0)
	txOperations := make(map[string]map[string]model.Instruction)
	operationIDs := make(map[string]struct{})
	for _, block := range a.OrderedBlocks {
		if block.ID == "" {
			return invalidArtifact("block id is required")
		}
		if _, exists := blockIDs[block.ID]; exists {
			return invalidArtifact("duplicate block id %q", block.ID)
		}
		blockIDs[block.ID] = struct{}{}
		if _, exists := logicalIDs[block.ID]; exists {
			return invalidArtifact("duplicate logical id %q", block.ID)
		}
		logicalIDs[block.ID] = struct{}{}
		for _, transaction := range block.Transactions {
			if transaction.ID == "" {
				return invalidArtifact("transaction id is required")
			}
			if _, exists := txIDs[transaction.ID]; exists {
				return invalidArtifact("duplicate transaction id %q", transaction.ID)
			}
			txIDs[transaction.ID] = block.ID
			if _, exists := logicalIDs[transaction.ID]; exists {
				return invalidArtifact("duplicate logical id %q", transaction.ID)
			}
			logicalIDs[transaction.ID] = struct{}{}
			txOrder = append(txOrder, transaction.ID)
			operations := make(map[string]model.Instruction, len(transaction.Program.Instructions))
			for _, instruction := range transaction.Program.Instructions {
				if instruction.ID == "" {
					return invalidArtifact("transaction %q has an operation without id", transaction.ID)
				}
				if _, exists := operationIDs[instruction.ID]; exists {
					return invalidArtifact("duplicate operation id %q", instruction.ID)
				}
				operationIDs[instruction.ID] = struct{}{}
				if _, exists := logicalIDs[instruction.ID]; exists {
					return invalidArtifact("duplicate logical id %q", instruction.ID)
				}
				logicalIDs[instruction.ID] = struct{}{}
				operations[instruction.ID] = instruction
			}
			txOperations[transaction.ID] = operations
		}
	}

	if len(a.LogicalArrivalSchedule) != len(txOrder) {
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

	if len(a.GroundTruth) != len(txOrder) {
		return invalidArtifact("ground truth count does not match transaction count")
	}
	for index, truth := range a.GroundTruth {
		if truth.TransactionID != txOrder[index] {
			return invalidArtifact("ground truth must follow ordered transaction order")
		}
		if !validTxStatus(truth.ExpectedStatus) {
			return invalidArtifact("ground truth for %q has invalid expected status", truth.TransactionID)
		}
		operations := txOperations[truth.TransactionID]
		for _, operationID := range truth.OperationPath {
			if _, exists := operations[operationID]; !exists {
				return invalidArtifact("ground truth path references unknown operation %q", operationID)
			}
		}
		for _, access := range truth.Accesses {
			instruction, exists := operations[access.OperationID]
			if !exists || !accessMatchesOpcode(access.Mode, instruction.Op) || !bytes.Equal(access.Key, instruction.Key) {
				return invalidArtifact("ground truth access does not match operation %q", access.OperationID)
			}
		}
		for _, branch := range truth.Branches {
			if operations[branch.BranchID].Op != model.OpJumpIf {
				return invalidArtifact("ground truth branch references non-branch operation %q", branch.BranchID)
			}
		}
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
		if record.Size != uint64(len(record.Payload)) {
			return invalidArtifact("metadata %q size mismatch", record.ID)
		}
		digest := sha256.Sum256(record.Payload)
		if record.Hash != hex.EncodeToString(digest[:]) {
			return invalidArtifact("metadata %q hash mismatch", record.ID)
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

func validTxStatus(status model.TxStatus) bool {
	switch status {
	case model.TxStatusSuccess,
		model.TxStatusFailed,
		model.TxStatusOutOfGas,
		model.TxStatusInvalidProgram,
		model.TxStatusInvalidState,
		model.TxStatusArithmeticError,
		model.TxStatusCancelled:
		return true
	default:
		return false
	}
}

func accessMatchesOpcode(mode AccessMode, opcode model.Opcode) bool {
	switch mode {
	case AccessRead:
		return opcode == model.OpRead
	case AccessWrite:
		return opcode == model.OpWrite
	case AccessDelete:
		return opcode == model.OpDelete
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
