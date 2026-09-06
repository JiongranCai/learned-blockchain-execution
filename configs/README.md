# Experiment configuration contracts

`experiment-matrix-v8` is accepted by `bench run` and `bench validate`. The runner checks configuration syntax and supported execution combinations, and records the resolved defaults for omitted dependency and kernel controls. `bench run` performs serial differential validation automatically, then measures the cases in fresh processes. Outputs are validation records, run records, and optional detailed traces; there is no validation bundle or expected workload hash.

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

Dependency acquisition, representation, and legacy scheduling use are represented by separate fields so one stage's cost cannot be hidden inside a mechanism label:

- `dependency_source ∈ {runtime_observed, static_program}` selects acquisition. `static_program` scans only engine-visible transaction programs inside the timed interval and never reads workload ground truth.
- `dependency_representation ∈ {version_only, raw_last_writer, max_raw_predecessor, full_conflict_graph}` selects the materialized structure without selecting a consumer.
- `dependency_representation_builder ∈ {none, indexed_by_key, quadratic_reference}` records how the structure is built. `version_only` requires `none`; RAW/summary representations use `indexed_by_key`; a full graph permits either the diagnostic quadratic reference or the correctness-equivalent key-indexed builder.
- `dependency_wait_policy ∈ {none, direct_predecessor_wait, contiguous_frontier_wait, all_predecessors_wait}` selects the representation consumer.
- `dependency_dispatch ∈ {index_order, ready_queue}` selects how that consumer is applied. `index_order` is the frozen behaviour: the transaction is dispatched to a worker and the gate blocks at `TxExecutor` entry, so a waiting transaction occupies a worker. `ready_queue` defers a transaction whose predecessors are incomplete instead of dispatching it, and releases it when they finish, which is the scheduling semantics a dependency DAG actually describes. Ready-queue dispatch requires a wait consumer and reports its work through `kernel_policy.dispatch_deferrals` instead of the gate wait counters.
- `dependency_estimate_injection ∈ {disabled, write_estimates}` independently controls static ESTIMATE locations supplied to the frozen kernel.
- `estimate_read_policy ∈ {suspend_in_place, abort_and_reschedule, suspend_yield_worker}` selects what a transaction does when a read observes an ESTIMATE mark. `suspend_in_place` is the frozen upstream behaviour: the partial execution and the worker are both retained for the whole wait. `abort_and_reschedule` is the Block-STM paper behaviour: the incarnation is discarded and the worker is freed, paying a full re-execution. `suspend_yield_worker` retains the partial execution and releases the worker for the duration of the wait.
- `idle_wait_policy ∈ {gosched, park}` selects what a worker does when the scheduler has no task. `gosched` is the frozen upstream busy-wait; `park` blocks until scheduler state changes. `suspend_yield_worker` forces `park`, because freed workers that spin consume the capacity the policy is meant to release.
- `dependency_mode ∈ {mvcc_runtime, declared_dag, summary, full_graph}` is retained for the legacy consumer bundles. New CQ3-R/U cases fix it to `mvcc_runtime`; legacy guided modes require their historical representation, wait, estimate, and `static_program` source combination.

CQ3-I telemetry records when the source becomes available, its implementation version, whether the artifact enters the runtime kernel, is discarded, or feeds a representation, and the existing completeness/exactness and acquisition cost counters. No content hash is computed for the transient information artifact.

CQ3-R telemetry records representation kind and builder, build time and deterministic work units, entries/edges, maximum fan-in, logical bytes, and process RSS. Write-estimate build time is recorded separately for legacy bundles. A representation-only case must report `representation_built_then_discarded` and zero plan lookups, resolution, estimate payload, and dependency waits.

CQ3-U telemetry records the resolved wait and estimate consumers, gate lookups/traversal/resolution, actual waits, estimate build/payload, and remaining reexecution work. A wait-only case must have zero estimate payload; an estimates-only case must have zero plan lookups and dependency waits.

The three kernel policies change only when work happens, never what a block commits. All three fields are optional. An omitted field resolves to the non-blocking behaviour when it is available — `abort_and_reschedule`, `park`, and `ready_queue` for a plan that has a wait consumer — and falls back to the frozen upstream value when it is not. An explicit field is never downgraded: an explicit non-frozen policy with a finite `max_speculative_inflight` is refused, because the admission limiter keeps separate stable-frontier bookkeeping; an explicit `ready_queue` without a wait consumer is refused because there is nothing to gate on; and an explicit `suspend_yield_worker` with `idle_wait_policy=gosched` is refused because a freed worker that spins consumes the capacity that policy exists to release. Omitting `idle_wait_policy` under `suspend_yield_worker` resolves it to `park`. The resolved plan is written back into the case, so every run record names the behaviour that actually executed. A case that resolves to the frozen triple routes through the untouched upstream entry point and reports `kernel_policy.applied=false` with zero policy counters. Range reads keep frozen suspend-in-place semantics, so the non-default estimate policies are unavailable to iterating transactions; the deterministic flat runtime never iterates.

Kernel policy telemetry reports what a run actually did: `estimate_suspends` / `estimate_suspend_ns`, `estimate_aborts`, `dispatch_deferrals`, `ready_queue_dispatches`, `worker_yields`, `idle_parks`, and `peak_runnable_workers`. An attempt discarded by `abort_and_reschedule` is charged the work it consumed and is reported as a replay with reason `estimate_dependency_abort`, not as a validation failure, so `validation_failures` stays specific to validation. `peak_runnable_workers` bounds how far `suspend_yield_worker` exceeded the configured executor count while replacing parked workers; a run whose peak is far above the executor count is comparing two things at once and should be read with that in mind.

Because a finite window falls back to the frozen kernel while a full window does not, a matrix that mixes finite and full `max_speculative_inflight` values now varies two things at once. Every CQ2 matrix under `experiments/speculation-window/` and the `speculation-interaction` matrix are in this state and must be re-run — either with the kernel policy pinned explicitly across all arms, or after the policy scheduler supports a finite window. Their existing records remain valid for the frozen kernel they were produced under.

Static program accesses are conservative syntactic sets. The current flat runtime gives complete coverage of every named state access, but branches, failures, gas exhaustion, or state errors can make the executed set smaller. Extra keys can delay work, while missing guidance is repaired by Block-STM validation and deterministic reexecution.

## Measurement boundary

The performance interval begins immediately before `Engine.ExecuteBlock` and ends immediately after it returns. State materialization and engine/policy construction happen before the interval. Execution-result digest computation and comparison, JSON encoding, and trace output happen afterward. State publication remains inside the interval.

The parent runner balances case order once per round with the frozen `order_seed`. Every scheduled case runs in a fresh worker process. Warmup records are retained and marked `phase=warmup`; analyses exclude them from measurement summaries rather than deleting them. `counters` is the formal low-cost telemetry mode, `detailed` captures action-level diagnostics, and `off` supports telemetry-overhead ablations.

Reproducibility records retain the Git revision and modified flag, config path, generator version and seed, hardware, and run settings. Preserve the config alongside the results. Config, binary, protocol, schema, and workload integrity hashes are not required. v7 configs and v1 workload descriptors belong to historical revisions; current benchmark/validation records use v8, while the unchanged action-trace format stays at v7.

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

`experiments/baseline/` exercises the serial oracle, Block-STM adapter, telemetry modes, and isolated runner process. Its Linux formal template remains intentionally invalid until target-host controls are frozen.

`experiments/speculation-window/` freezes `P=8` and compares the distinct effective admission choices `1/P/4P/W`. The anchor matrices contrast expensive low-conflict work with a cheap single-key hotspot chain. Boundary matrices keep the seed, transaction count, compute distribution, workers, and all other controls fixed while changing only `key_space` from 1 through 3. The hotspot/cold-tail matrix keeps `key_space=8192` while concentrating accesses on a small hot head and explicitly controlling read/write correlation.

`experiments/dependency-acquisition/` is the isolated CQ3-I family. Its two smoke anchors and Linux formal template keep `P=8`, `L=W`, `dependency_mode=mvcc_runtime`, runtime validation/reexecution, and all representation consumers fixed. Only `dependency_source` changes between `runtime_observed` and `static_program`; the latter must report `discarded_after_acquisition`, zero representation time, and zero plan lookups.

`experiments/dependency-representation/` is the isolated CQ3-R family. It fixes `dependency_source=static_program`, `dependency_mode=mvcc_runtime`, `P=8`, and `L=W`, then compares version-only, RAW last-writer, max-RAW predecessor, and the two full-conflict-graph builders. Both smoke anchors use 1 warmup and 5 measurement rounds for local feasibility only. The Linux template uses the formal 3/30 protocol and intentionally rejects placeholder host controls until instantiated on the target machine.

`experiments/dependency-consumer/` is the isolated CQ3-U family. It fixes `dependency_source=static_program`, `dependency_mode=mvcc_runtime`, the key-indexed builder, `P=8`, and `L=W`. Within each representation, matched pairs change only the wait consumer; the RAW estimates pair changes only estimate injection. Priority and early-validation consumers remain unavailable because the frozen scheduler and MVCC callbacks do not expose the required hooks.

`experiments/kernel-policy/` isolates the three Block-STM blocking behaviours. It fixes `P=8`, `L=W`, the key-indexed builder, and one `selective_read_set` workload whose conservative static read set produces many false predecessors. Matched contrasts change exactly one thing: `direct`/`frontier` waits under `index_order` versus `ready_queue` dispatch, and write estimates under the three `estimate_read_policy` values. The Linux formal template intentionally rejects placeholder host controls until instantiated on the target machine.

`experiments/dependency-guidance/` contains three comparison families:

- hotspot, low-conflict, and state-dependent-branch anchors compare runtime MVCC, direct declared RAW dependencies, a compact predecessor summary, and a full preset-order RAW/WAR/WAW graph;
- the equal-information ablation scans the same static programs in every case and compares version-only use, summary, and full graph;
- the speculation interaction matrix crosses `L∈{1,W}` with `{runtime MVCC, declared DAG}` on one frozen workload.

Telemetry reports acquisition time and bytes, representation time and logical bytes, graph edges or summary entries, estimate payload, plan traversal, dependency waiting, remaining reexecution work, and process RSS. Acquisition and representation are sequential block-planning wall times; resolution and waiting are summed transaction-callback worker times and can exceed block latency. Saved invalid work is a paired delta against the matching runtime baseline, not a directly observed per-run counter.

After a dependency-guidance run, `scripts/summarize_dependency_guidance.sh` emits a measurement-only TSV of medians and mechanism costs. It excludes warmups, failed or censored records, and canonical mismatches; inferential statistics must still follow the frozen protocol.
