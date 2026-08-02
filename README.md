# Learned Fine-Grained Blockchain Execution

This project explores how learning-based methods can improve blockchain transaction execution by adapting execution strategies to workload characteristics. It aims to identify workload-dependent trade-offs among different execution mechanisms and develop a fine-grained adaptive execution engine that improves performance and resource efficiency while preserving deterministic and correct transaction outcomes.

The long-term goal is to move beyond fixed or coarse-grained execution policies and enable more precise execution decisions for heterogeneous and dynamically changing workloads.

## Frozen execution substrate

The implementation starts from [`crypto-org-chain/go-block-stm`](https://github.com/crypto-org-chain/go-block-stm), frozen at commit `7afe924fb4a611a2626f92338f1f76e4ebefa62f` (2024-12-13). The upstream Git history is retained through the `go-block-stm-upstream` remote and the merge commit that imported the code.

The frozen upstream module:

- implements the Block-STM algorithm with a preset-order, multi-version execution model;
- exposes `ExecuteBlock` as its primary API;
- integrates Cosmos SDK `MultiStore`, including deletion and iteration support;
- suspends a transaction on an `ESTIMATE` read and resumes it after the blocking incarnation finishes;
- is licensed under Apache License 2.0 and declares Go 1.21.

```go
type TxExecutor func(TxnIndex, MultiStore)

func ExecuteBlock(
	ctx context.Context,
	blockSize int,
	stores map[storetypes.StoreKey]int,
	storage MultiStore,
	executors int,
	txExecutor TxExecutor,
) error
```

The imported code is the experimental kernel, not the final framework. New policy hooks, deterministic workloads, serial differential validation, telemetry, capability guards, and fine-grained recovery will be layered around the shared safety kernel.

## Serial semantics

The Week 2 reference path is implemented under `internal/` without modifying the frozen upstream kernel:

- `model` defines typed blocks, transactions, flat instructions, results, and the schema-versioned canonical digest;
- `state/memkv` provides cloned byte ownership, key-sorted snapshots, and transaction-local overlays;
- `runtime/flat` executes deterministic integer programs with reads, writes, deletes, fixed-cost compute, conditions, jumps, explicit failure, and return;
- `engine/serial` executes transactions in preset order and is the correctness oracle for later parallel engines;
- `workload` defines the strict, hash-sealed `WorkloadArtifact v1` and its restricted engine view;
- `workload/synthetic` materializes initial state, ordered blocks, logical arrivals, stable operation IDs, and audit ground truth from a seed.

The frozen execution contract is:

- state keys are arbitrary bytes; runtime values are signed 64-bit integers encoded as exactly eight big-endian bytes;
- a missing read yields integer zero with `Exists=false`; malformed stored values fail with `invalid_state`;
- each instruction costs one unit and `compute(n)` costs `n+1`; checked arithmetic, gas exhaustion, and invalid programs have distinct stable statuses/codes;
- compute work updates the result-visible `ComputeDigest`, which is covered by the canonical result digest and prevents dead-work elimination;
- transactions read their own writes, and the last operation on a key wins within that transaction;
- only `success` commits the transaction overlay; explicit failure, invalid program/state, arithmetic failure, and out-of-gas retain accounting/reads but publish no writes;
- semantic transaction failure does not abort the block; infrastructure cancellation aborts the block and publishes none of that block's state;
- state snapshots and write sets are byte-key sorted, while transaction/read/event order remains execution order; the canonical SHA-256 digest excludes its own `Digest` field.

Every generated workload is sealed as `workload-artifact-v1`. Its canonical hash covers generator name/version/config/seed, initial state, ordered programs, logical arrival schedule, stable IDs, audit-only actual access/path metadata, and engine-visible metadata records. Candidate execution receives an `ExecutionInput`, which cannot represent ground truth and exposes metadata only when its declared source is explicitly allowed. Descriptor parsing rejects unknown fields, stale hashes, malformed metadata size/hash, and inconsistent logical IDs before execution.

## Block-STM adapter and fixed policies

Week 3 adds the unified execution seam without changing the frozen root-level scheduler:

- `internal/engine` defines the common engine/run configuration and atomic state-publication contract;
- `internal/control` registers all 19 first-version events, their unique typed dispatch keys, action schemas, trust classes, capabilities, and logical traces;
- `internal/policy` groups macro, transaction, runtime, and recovery hooks behind one immutable policy identity;
- `internal/policy/fixed` provides explicit `SerialPreset` and `BlockSTMPreset` decisions for every hook;
- `internal/engine/blockstm` adapts the flat runtime and byte state to the original Block-STM MVCC/scheduler API, publishing only a completed canonical block snapshot.

The adapter preserves missing versus existing empty values with a private value frame. Failed speculative executions never publish their overlay writes. Final transaction results remain in preset order, and validation-driven reexecution is represented by increasing incarnations plus paired `VALIDATION_FAIL`/`REPLAY_START` events. Kernel-internal callbacks that the frozen `TxExecutor` API cannot expose—such as the precise `READ_ESTIMATE`, conflict, worker-idle, and queue-pressure events—are declared unavailable with reasons instead of being fabricated.

The differential suite compares the complete canonical `BlockResult` and final state, including ordered transaction status, errors, units, reads, writes, return values, runtime events, compute digest, and block digest. It covers rollback, delete, malformed state, out-of-gas, arithmetic overflow, branches, hot-key conflicts, multiple seeds, and executor counts.

Run the macOS correctness gate from the repository root:

```sh
go test -count=1 ./...
go test -race -count=1 ./...
go vet ./...
```

Repeat the determinism-sensitive packages with:

```sh
go test -count=20 ./internal/...
```

To inspect the Week 3 gates directly:

```sh
go test -count=1 -v ./internal/engine/blockstm
go test -count=1 -v ./internal/control ./internal/policy ./internal/policy/fixed
go test -race -count=1 ./internal/engine/blockstm ./internal/control ./internal/policy
```

`scripts/verify_upstream_baseline.sh` additionally proves that every original frozen root-level go-block-stm file remains unmodified and reruns the full baseline suite. Week 5 adds `speculation.go` beside those files because the admission scheduler must share package-private MVMemory/status machinery; the `L=W` default still calls the original `ExecuteBlock` path directly. Performance, scaling, affinity, and NUMA measurements are intentionally deferred to Linux; macOS results are correctness checks and pilot evidence only.

## Benchmark runner and telemetry

Week 4 adds a strict experiment-matrix runner under `cmd/bench`; Week 5 evolves the contract to `experiment-matrix-v2` with an explicit CQ2 `max_speculative_inflight` budget. Build the binary so Go embeds the Git revision and dirty-worktree bit, then run the smoke matrix:

```sh
go build -trimpath -o /tmp/blockchain-execution-bench ./cmd/bench
/tmp/blockchain-execution-bench validate -config configs/experiments/week4-smoke.json
/tmp/blockchain-execution-bench run -config configs/experiments/week4-smoke.json
```

`validate` executes the serial oracle and every configured candidate on independent states, compares the complete canonical block results, and writes a validation bundle bound to the binary commit, config hash, statistical protocol, workload hash, and expected result digest. `run` refuses a stale bundle and executes only a validated candidate. Every warmup and measurement is launched in a fresh process, while workload loading/generation, state construction, canonical digesting, result comparison, JSON encoding, and trace output remain outside the timed interval.

Each run emits `benchmark-run-v2` JSONL with hardware/runtime provenance, process ID, Linux allowed CPU/memory-node lists and governor, block latencies, goodput, useful/re-executed/discarded work, incarnation attempts, CQ2 limit/peak/stall counters, action/fallback counters, policy decision cost, memory high-water mark, capability availability, and explicit unavailable metrics. `action-trace-v2` records stable targets, trust class, feature source, observation version, policy-table version, action, and lookup/dispatch duration. Trace modes are `detailed`, `counters`, and `off`; formal timing uses `counters`, while `detailed` is a diagnostic/action-trace mode. Matched instrumented/off cases produce a `telemetry-ablation-v1` record without changing canonical results.

The smoke outputs are generated under `results/` and are intentionally ignored by Git. They are not paper data. Formal runs require Linux, a clean VCS-stamped binary, the frozen minimum repetitions in `configs/statistical/protocol-v1.json`, and explicit affinity, NUMA, and page-cache controls. The formal template is deliberately rejected until its `REPLACE_WITH_...` fields are frozen for the target server.

On an already provisioned Linux host, the complete correctness/build/runner smoke is:

```sh
./scripts/run_week4_linux_smoke.sh
```

The script uses `GOPROXY=off` and never installs tools or dependencies; a missing Go toolchain, race prerequisite, or module cache is reported as an environment failure.

## Week 5 CQ2 speculation limit

`max_speculative_inflight` limits the number of admitted top-level transactions beyond the continuous stable validated frontier. A transaction retains one slot while executing, suspended, validating, or reexecuting; a new incarnation does not take a second slot. Positive limits use the admission-aware scheduler, while zero means `W` and preserves the original full-window Block-STM path.

The committed Mac smoke matrices keep `P=8`, CQ1 at one full consensus block, CQ3 at runtime MVCC, CQ4 at whole-transaction recovery, and CQ5 on the shared worker pool. They compare `L ∈ {1, P, 4P, W}` on expensive low-conflict, cheap hotspot-chain, and fixed-cost boundary workloads with `key_space ∈ {1,2,3}`. Every candidate is differentially validated against the serial oracle before measurement.

```sh
./scripts/run_week5_cq2_smoke.sh
```

`benchmark-run-v2` reports the configured/effective limit, exact peak admitted occupancy and summed admission-stall worker time for finite limits, plus the existing incarnation/reexecution/discarded-work breakdown. Exact occupancy is deliberately unavailable on the untouched `L=W` path rather than inferred. The Linux formal template remains rejected until its affinity, NUMA, and page-cache placeholders are frozen on the target host.
