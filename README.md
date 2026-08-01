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
