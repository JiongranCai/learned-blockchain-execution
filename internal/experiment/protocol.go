package experiment

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
)

const StatisticalProtocolSchemaVersion = "statistical-protocol-v1"

var ErrInvalidStatisticalProtocol = errors.New("invalid statistical protocol")

type StatisticalProtocol struct {
	SchemaVersion                string   `json:"schema_version"`
	Status                       string   `json:"status"`
	PairedWorkloadSeeds          bool     `json:"paired_workload_seeds"`
	BalancedRandomizedOrder      bool     `json:"balanced_randomized_order"`
	MinimumWarmupRounds          int      `json:"minimum_warmup_rounds"`
	MinimumMeasurementRounds     int      `json:"minimum_measurement_rounds"`
	ConfidenceLevel              float64  `json:"confidence_level"`
	ConfidenceIntervalMethod     string   `json:"confidence_interval_method"`
	BootstrapResamples           int      `json:"bootstrap_resamples"`
	MaterialEffectRatio          float64  `json:"material_effect_ratio"`
	TelemetryOverheadBudgetRatio float64  `json:"telemetry_overhead_budget_ratio"`
	P99MinimumTransactions       int      `json:"p99_minimum_transactions"`
	MultipleComparisonCorrection string   `json:"multiple_comparison_correction"`
	OutlierPolicy                string   `json:"outlier_policy"`
	TimeoutPolicy                string   `json:"timeout_policy"`
	CrashOOMPolicy               string   `json:"crash_oom_policy"`
	RankingReversalRequirements  []string `json:"ranking_reversal_requirements"`
	PilotSeparation              string   `json:"pilot_separation"`
}

type LoadedProtocol struct {
	Protocol StatisticalProtocol
	Hash     string
}

func LoadStatisticalProtocol(path string) (LoadedProtocol, error) {
	encoded, err := os.ReadFile(path)
	if err != nil {
		return LoadedProtocol{}, err
	}
	var protocol StatisticalProtocol
	if err := decodeStrict(encoded, &protocol); err != nil {
		return LoadedProtocol{}, fmt.Errorf("%w: %v", ErrInvalidStatisticalProtocol, err)
	}
	if err := protocol.Validate(); err != nil {
		return LoadedProtocol{}, err
	}
	canonical, err := json.Marshal(protocol)
	if err != nil {
		return LoadedProtocol{}, err
	}
	digest := sha256.Sum256(append([]byte(StatisticalProtocolSchemaVersion+"\x00"), canonical...))
	return LoadedProtocol{Protocol: protocol, Hash: hex.EncodeToString(digest[:])}, nil
}

func (p StatisticalProtocol) Validate() error {
	invalid := func(format string, args ...any) error {
		return fmt.Errorf("%w: %s", ErrInvalidStatisticalProtocol, fmt.Sprintf(format, args...))
	}
	if p.SchemaVersion != StatisticalProtocolSchemaVersion {
		return invalid("schema_version must be %q", StatisticalProtocolSchemaVersion)
	}
	if p.Status != "frozen" || !p.PairedWorkloadSeeds || !p.BalancedRandomizedOrder {
		return invalid("protocol must freeze paired seeds and balanced randomized order")
	}
	if p.MinimumWarmupRounds < 0 || p.MinimumMeasurementRounds <= 0 {
		return invalid("round minima are invalid")
	}
	if p.ConfidenceLevel <= 0 || p.ConfidenceLevel >= 1 || p.BootstrapResamples <= 0 {
		return invalid("confidence protocol is invalid")
	}
	if p.ConfidenceIntervalMethod == "" || p.MaterialEffectRatio <= 0 || p.MaterialEffectRatio >= 1 ||
		p.TelemetryOverheadBudgetRatio < 0 || p.TelemetryOverheadBudgetRatio >= 1 {
		return invalid("effect and overhead rules are required")
	}
	if p.P99MinimumTransactions <= 0 || p.MultipleComparisonCorrection == "" || p.OutlierPolicy == "" ||
		p.TimeoutPolicy == "" || p.CrashOOMPolicy == "" || p.PilotSeparation == "" {
		return invalid("handling rules must be explicit")
	}
	if len(p.RankingReversalRequirements) != 4 {
		return invalid("ranking reversal must have exactly four frozen requirements")
	}
	return nil
}

func (p StatisticalProtocol) ValidateRunClass(config Config) error {
	if config.RunClass != "formal" {
		return nil
	}
	if config.WarmupRounds < p.MinimumWarmupRounds || config.MeasurementRounds < p.MinimumMeasurementRounds {
		return fmt.Errorf("%w: formal run has %d/%d warmup/measurement rounds, requires at least %d/%d",
			ErrInvalidStatisticalProtocol,
			config.WarmupRounds,
			config.MeasurementRounds,
			p.MinimumWarmupRounds,
			p.MinimumMeasurementRounds,
		)
	}
	return nil
}
