package engine

import (
	"context"
	"errors"
	"fmt"
	"reflect"

	"github.com/crypto-org-chain/go-block-stm/internal/control"
	"github.com/crypto-org-chain/go-block-stm/internal/model"
	"github.com/crypto-org-chain/go-block-stm/internal/policy"
	"github.com/crypto-org-chain/go-block-stm/internal/state"
)

var (
	ErrNilState                = errors.New("engine requires a non-nil state")
	ErrMissingBlockID          = errors.New("block id is required")
	ErrInvalidWorkers          = errors.New("invalid executor count")
	ErrInvalidSpeculationLimit = errors.New("invalid max speculative inflight")
	ErrInvalidDependencyMode   = errors.New("invalid dependency control")
	ErrUnsupported             = errors.New("policy decision is unsupported by engine")
)

type RunConfig struct {
	Executors int
	EpochID   string
	Policy    policy.Policy
	TraceMode control.TraceMode
	// MaxSpeculativeInflight bounds admitted transactions beyond the stable
	// validated frontier. Zero means the full block window (W).
	MaxSpeculativeInflight int
	// DependencyMode and DependencySource independently record the legacy
	// representation/use and acquisition choices. Their zero values preserve
	// the runtime-MVCC baseline for direct engine callers.
	DependencyMode   control.DependencyMode
	DependencySource control.DependencySource
	OmitResultDigest bool
}

func EffectiveDependencyControl(config RunConfig) (control.DependencyMode, control.DependencySource, error) {
	mode := config.DependencyMode
	if mode == "" {
		mode = control.DependencyMVCCRuntime
	}
	if !control.ValidDependencyMode(mode) {
		return "", "", fmt.Errorf("%w: unknown mode %q", ErrInvalidDependencyMode, mode)
	}
	source := config.DependencySource
	if source == "" {
		if mode != control.DependencyMVCCRuntime {
			return "", "", fmt.Errorf("%w: mode %q requires explicit information source", ErrInvalidDependencyMode, mode)
		}
		source = control.DependencySourceRuntimeObserved
	}
	if !control.ValidDependencySource(source) {
		return "", "", fmt.Errorf("%w: unknown information source %q", ErrInvalidDependencyMode, source)
	}
	if mode != control.DependencyMVCCRuntime && source != control.DependencySourceStaticProgram {
		return "", "", fmt.Errorf("%w: mode %q requires %q information", ErrInvalidDependencyMode, mode, control.DependencySourceStaticProgram)
	}
	return mode, source, nil
}

func EffectiveTraceMode(config RunConfig) (control.TraceMode, error) {
	if config.TraceMode == "" {
		return control.TraceDetailed, nil
	}
	if !control.ValidTraceMode(config.TraceMode) {
		return "", fmt.Errorf("invalid trace mode %q", config.TraceMode)
	}
	return config.TraceMode, nil
}

type Engine interface {
	Name() string
	Capabilities() control.Capabilities
	ExecuteBlock(context.Context, model.Block, state.Store, RunConfig) (model.BlockResult, control.Trace, error)
}

func ValidateBlock(block model.Block, storage state.Store) error {
	if storage == nil || isNil(storage) {
		return ErrNilState
	}
	if block.ID == "" {
		return ErrMissingBlockID
	}
	seen := make(map[string]struct{}, len(block.Transactions))
	for index, transaction := range block.Transactions {
		if transaction.ID == "" {
			return fmt.Errorf("transaction %d has no id", index)
		}
		if _, exists := seen[transaction.ID]; exists {
			return fmt.Errorf("duplicate transaction id %q", transaction.ID)
		}
		seen[transaction.ID] = struct{}{}
	}
	return nil
}

func isNil(value any) bool {
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

func EpochID(config RunConfig, block model.Block) string {
	if config.EpochID != "" {
		return config.EpochID
	}
	return block.ID
}

func Unsupported(event control.Event, action string) error {
	return fmt.Errorf("%w: event %s action %q", ErrUnsupported, event, action)
}
