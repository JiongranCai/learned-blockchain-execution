# Learned Fine-Grained Blockchain Execution

Research framework for learning workload-aware blockchain transaction execution strategies by decomposing protocols into configurable mechanisms: dependency acquisition, representation, dispatch, and conflict handling.

Built on [go-block-stm](https://github.com/crypto-org-chain/go-block-stm), with a deterministic transaction runtime, synthetic workload generator, serial reference engine, and experiment runner.

## Build and test

Requires Go 1.21 or later. From the repository root:

```sh
go build -trimpath -o /tmp/bench ./cmd/bench
go test ./...
```

The full test, race, and vet suite is available through `./scripts/verify_upstream_baseline.sh`.

## Experiments

Develop and test locally; run performance experiments on the Linux server.

```sh
/tmp/bench validate -config configs/experiments/workload/standard-smoke.json
/tmp/bench run -config configs/experiments/workload/standard-smoke.json
```

`validate` checks execution against the serial reference. `run` includes this check, then measures cases in fresh processes. Set output paths in the experiment config to a separate results directory on the server.

See [configuration and workload parameters](configs/README.md). Framework code is under `internal/`, the CLI under `cmd/bench/`, and experiment tools under `scripts/`. Source, tests, scripts, and experiment configs are tracked in Git; generated results are excluded.

## License

[Apache License 2.0](LICENSE).
