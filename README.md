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
type ExecuteFn func(TxnIndex, MultiStore)

func ExecuteBlock(
	ctx context.Context,
	blockSize int,
	stores []storetypes.StoreKey,
	storage MultiStore,
	executors int,
	executeFn ExecuteFn,
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

`scripts/verify_upstream_baseline.sh` additionally proves that the frozen root-level go-block-stm kernel is unchanged and reruns the full baseline suite. Performance, scaling, affinity, and NUMA measurements are intentionally deferred to Linux; macOS results are correctness checks only.
