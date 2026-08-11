# Learned Fine-Grained Blockchain Execution

This repository is an experimental framework for studying adaptive blockchain transaction execution. The project decomposes execution protocols into independently configurable mechanisms, measures their workload-dependent trade-offs, and provides the deterministic safety substrate needed to combine those mechanisms in a future learned policy.

The current implementation focuses on a reproducible motivation and systems-evaluation platform. It does not yet contain the learned policy itself.

## Current implementation

- A frozen [`crypto-org-chain/go-block-stm`](https://github.com/crypto-org-chain/go-block-stm) execution kernel at commit `7afe924fb4a611a2626f92338f1f76e4ebefa62f`.
- A deterministic flat transaction runtime, in-memory state implementation, and preset-order serial oracle.
- A common engine and policy interface shared by serial execution and Block-STM.
- Seeded synthetic workloads with hash-sealed artifacts, configurable uniform or hotspot/cold-tail key-access distributions, stable operation identifiers, state-dependent branches, and an explicit boundary between engine-visible inputs and audit-only ground truth.
- A configurable speculation window through `max_speculative_inflight`.
- Dependency controls that expose CQ3-I acquisition and CQ3-R representation as separate stages:
  - `runtime_observed` uses the mandatory MVCC runtime path;
  - `static_program` scans engine-visible programs before execution; with `mvcc_runtime` the acquired artifact is measured and discarded;
  - `version_only`, `raw_last_writer`, `max_raw_predecessor`, and `full_conflict_graph` select independently measured representations;
  - `full_conflict_graph` has both a quadratic diagnostic builder and a correctness-equivalent key-indexed builder;
  - legacy `declared_dag`, `summary`, and `full_graph` scheduling bundles remain available for historical reproduction, while CQ3-R builds representations without a static consumer.
- Differential validation against the serial oracle before a candidate can be benchmarked.
- Schema-versioned experiment matrices, isolated worker processes, provenance records, action traces, and mechanism-specific telemetry.

Static dependency information is treated only as an optimization hint. Guided modes preserve preset transaction order and retain Block-STM read-set validation, deterministic reexecution, and atomic final-state publication. Missing or imprecise guidance can reduce performance, but cannot bypass the correctness path.

## Repository layout

```text
cmd/bench/                   experiment runner CLI
configs/                     experiment and statistical contracts
internal/control/            control types, events, traces, and counters
internal/engine/             serial and Block-STM engine adapters
internal/experiment/         validation and benchmark orchestration
internal/model/              canonical blocks, transactions, and results
internal/policy/             policy interfaces and fixed presets
internal/runtime/            deterministic transaction runtime
internal/state/              state abstractions and in-memory store
internal/telemetry/          provenance and measurement records
internal/workload/           workload artifacts and generators
scripts/                     verification, smoke-run, and summary tools
```

Root-level Go files come from the frozen Block-STM substrate, except for explicitly additive integration files such as `speculation.go`. New framework code lives primarily under `internal/` so the upstream safety kernel remains auditable.

## Build and verify

The module declares Go 1.21 or later. Run the standard correctness gates from the repository root:

```sh
go test -count=1 ./...
go test -race -count=1 ./...
go vet ./...
./scripts/verify_upstream_baseline.sh
```

The baseline verifier checks the frozen upstream source, runs the full test and race suites, repeats determinism-sensitive execution, and executes a small Block-STM benchmark.

## Run experiments

Build the benchmark runner and validate a smoke matrix before executing it:

```sh
go build -trimpath -o /tmp/blockchain-execution-bench ./cmd/bench
/tmp/blockchain-execution-bench validate -config configs/experiments/baseline/smoke.json
/tmp/blockchain-execution-bench run -config configs/experiments/baseline/smoke.json
```

Convenience scripts cover the implemented comparison families:

```sh
./scripts/run_baseline_linux_smoke.sh
./scripts/run_speculation_window_smoke.sh
./scripts/run_dependency_guidance_smoke.sh
./scripts/run_dependency_representation_smoke.sh
./scripts/summarize_dependency_guidance.sh
```

CQ3-I acquisition-only smoke matrices live under `configs/experiments/dependency-acquisition/`. They hold `dependency_mode=mvcc_runtime`, `max_speculative_inflight=W`, and every consumer fixed while comparing `runtime_observed` with `static_program` acquisition paid then discarded.

CQ3-R representation-only matrices live under `configs/experiments/dependency-representation/`. They hold `dependency_source=static_program`, `dependency_mode=mvcc_runtime`, `max_speculative_inflight=W`, and every static consumer disabled while comparing the representation and builder fields.

Smoke runs are correctness checks and pilot evidence. Formal performance runs belong on a controlled Linux server with frozen CPU affinity, NUMA policy, page-cache policy, toolchain, and statistical protocol. The committed formal templates intentionally reject placeholder environment controls.

Generated results, temporary files, local papers, research notes, and private development artifacts are excluded from Git. The public repository tracks the source, tests, scripts, and reproducible experiment contracts required to rebuild results.

## License

The imported Block-STM substrate and this repository are distributed under the Apache License 2.0. See [LICENSE](LICENSE).
