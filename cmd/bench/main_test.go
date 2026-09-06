package main

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/crypto-org-chain/go-block-stm/internal/experiment"
	"github.com/crypto-org-chain/go-block-stm/internal/telemetry"
)

func TestMain(m *testing.M) {
	// Let the test executable serve as the CLI's real subprocess worker.
	if len(os.Args) > 1 && os.Args[1] == "_worker" {
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestRunValidatesAndUsesFreshWorkerProcesses(t *testing.T) {
	loaded, err := experiment.LoadConfig("../../configs/experiments/baseline/smoke.json")
	if err != nil {
		t.Fatal(err)
	}
	config := loaded.Config
	directory := t.TempDir()
	config.StatisticalProtocol, err = filepath.Abs("../../configs/statistical/protocol-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	config.WarmupRounds, config.MeasurementRounds = 0, 1
	config.TelemetryAblation = nil
	config.Output = experiment.OutputConfig{
		ValidationRecords: filepath.Join(directory, "validation.jsonl"),
		RunRecords:        filepath.Join(directory, "runs.jsonl"),
		ActionTraces:      filepath.Join(directory, "traces.jsonl"),
	}
	encoded, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "config.json")
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"run", "-config", path}); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(config.Output.RunRecords)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	pids := make(map[int]bool)
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var record telemetry.BenchmarkRecord
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			t.Fatal(err)
		}
		pid := record.Provenance.ProcessID
		if !record.CanonicalMatch || record.Status != "success" || pid == os.Getpid() || pid == 0 || pids[pid] {
			t.Fatalf("incorrect result or reused process: %#v", record)
		}
		pids[pid] = true
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if len(pids) != len(config.Cases) {
		t.Fatalf("got %d workers, want %d", len(pids), len(config.Cases))
	}
}
