# Experiment configuration contracts

`experiment-matrix-v3` is the only input contract accepted by `bench validate` and `bench run`. Parsing rejects unknown fields, duplicate case identifiers, unsupported engines or policies, invalid trace modes, ambiguous workload sources, unfrozen workload hashes, implicit environment controls, negative speculation limits, invalid serial controls, illegal dependency mode/information pairs, and formal configurations with placeholder affinity, NUMA, or page-cache values.

## Execution controls

`max_speculative_inflight` is the static admission budget. Zero selects the original full-block window `W`; a positive value is reduced to `min(L,W)` for each block. The worker count remains fixed while `L` changes. A transaction occupies one slot until it enters the continuous stable validated frontier, including suspension and every incarnation; reexecution does not acquire another slot.

Synthetic workloads default to the legacy uniform key distribution. Setting
`access_distribution.kind` to `hotspot` divides the configured `key_space`
into the first `hot_key_count` hot keys and a cold tail; each read and write
independently selects the hot set with `hot_access_probability`, then samples
uniformly inside the selected set. This expresses a large address space with a
small hot working set without collapsing all cold keys into the hotspot.
`min_compute_units` optionally changes the legacy `[0, max_compute_units]`
uniform compute range to `[min_compute_units, max_compute_units]`; setting the
minimum equal to the maximum produces fixed-cost transactions and separates
compute-time skew from access skew.
`access_distribution.read_write_same_key_probability` controls read/write
correlation independently of hot-set selection. Zero preserves independent
sampling; one models a read-modify-write against the same selected key.

Dependency acquisition and dependency use are represented by separate fields so information cost cannot be hidden inside a mechanism label:

- `dependency_information ∈ {runtime_observed, static_program}` selects acquisition. `static_program` scans only engine-visible transaction programs inside the timed interval and never reads workload ground truth.
- `dependency_mode ∈ {mvcc_runtime, declared_dag, summary, full_graph}` selects representation and scheduling use. Every guided mode requires `static_program`. The `mvcc_runtime/static_program` pair is an equal-information version-only ablation: it pays for the same scan but deliberately discards the static access sets.

Static program accesses are conservative syntactic sets. The current flat runtime gives complete coverage of every named state access, but branches, failures, gas exhaustion, or state errors can make the executed set smaller. Extra keys can delay work, while missing guidance is repaired by Block-STM validation and deterministic reexecution.

## Measurement boundary

The performance interval begins immediately before `Engine.ExecuteBlock` and ends immediately after it returns. State materialization and engine/policy construction happen before the interval. Canonical hashing, expected-digest comparison, JSON encoding, and trace output happen afterward. State publication remains inside the interval.

The parent runner balances case order once per round with the frozen `order_seed`. Every scheduled case runs in a fresh worker process. Warmup records are retained and marked `phase=warmup`; analyses exclude them from measurement summaries rather than deleting them. `counters` is the formal low-cost telemetry mode, `detailed` captures action-level diagnostics, and `off` supports telemetry-overhead ablations.

## Statistical protocol

`statistical/protocol-v1.json` freezes the formal analysis protocol:

- paired workload seeds and randomized, round-balanced case order;
- at least 3 warmup and 30 measurement rounds;
- paired percentile bootstrap with 10,000 resamples and 95% confidence;
- a 5% material-effect threshold and 10% median telemetry-overhead budget;
- Holm–Bonferroni correction within each preregistered comparison family;
- no discretionary successful-run outlier deletion;
- explicit timeout, crash, and OOM records with raw diagnostics preserved;
- at least 10,000 transactions before reporting p99;
- ranking reversal only when the paired interval excludes zero, the material threshold is exceeded, nearby workload points repeat the reversal, and switching, policy, and telemetry overhead are included.

Smoke matrices may use fewer rounds, but their records remain pilot evidence and cannot be pooled with formal data.

## Experiment families

`experiments/baseline/` exercises the serial oracle, Block-STM adapter, validation bundle, telemetry modes, and isolated runner process. Its Linux formal template remains intentionally invalid until target-host controls are frozen.

`experiments/speculation-window/` freezes `P=8` and compares the distinct effective admission choices `1/P/4P/W`. The anchor matrices contrast expensive low-conflict work with a cheap single-key hotspot chain. Boundary matrices keep the seed, transaction count, compute distribution, workers, and all other controls fixed while changing only `key_space` from 1 through 3. The hotspot/cold-tail matrix keeps `key_space=8192` while concentrating accesses on a small hot head and explicitly controlling read/write correlation.

`experiments/dependency-guidance/` contains three comparison families:

- hotspot, low-conflict, and state-dependent-branch anchors compare runtime MVCC, direct declared RAW dependencies, a compact predecessor summary, and a full preset-order RAW/WAR/WAW graph;
- the equal-information ablation scans the same static programs in every case and compares version-only use, summary, and full graph;
- the speculation interaction matrix crosses `L∈{1,W}` with `{runtime MVCC, declared DAG}` on one frozen workload.

Telemetry reports acquisition time and bytes, representation time and logical bytes, graph edges or summary entries, estimate payload, plan traversal, dependency waiting, remaining reexecution work, and process RSS. Acquisition and representation are sequential block-planning wall times; resolution and waiting are summed transaction-callback worker times and can exceed block latency. Saved invalid work is a paired delta against the matching runtime baseline, not a directly observed per-run counter.

After a dependency-guidance run, `scripts/summarize_dependency_guidance.sh` emits a measurement-only TSV of medians and mechanism costs. It excludes warmups, failed or censored records, and canonical mismatches; inferential statistics must still follow the frozen protocol.
