# Experiment configuration contracts

`experiment-matrix-v8` is accepted by `bench run` and `bench validate`. The runner checks configuration syntax and supported execution combinations, and records the resolved defaults for omitted dependency and kernel controls. `bench run` performs serial differential validation automatically, then measures the cases in fresh processes. Outputs are validation records, run records, and optional detailed traces; there is no validation bundle or expected workload hash.

## Execution controls

`max_speculative_inflight` is the static admission budget. Zero selects the full-block window `W`; a positive value is reduced to `min(L,W)` for each block. The worker count remains fixed while `L` changes. A transaction occupies one slot until it enters the continuous stable validated frontier, including dependency deferral, suspension and every incarnation; reexecution does not acquire another slot. The policy kernel supports finite windows: only first admission is bounded, while validation and re-dispatch of admitted transactions can proceed. Out-of-order validation does not release a slot until the preceding transactions are stable. Static acquisition and representation construction still cover the whole block, so their scope does not change with L.

Finite-window telemetry records effective L, peak admitted positions beyond the stable prefix, and worker admission-stall events/time. This occupancy includes pending and validating transactions; it is not a count of running workers. `L=0` and `L>=W` use the existing full-window path, where exact occupancy/stall telemetry is unavailable.

## Synthetic workloads

`workload.synthetic` contains `seed`, `initial_keys`, `key_space`, `block_count`,
`transactions_per_block`, and a `mix` of transaction templates. Optional
`failure_every` injects semantic failures for rollback tests. The generator is
`synthetic-v4`, producing `workload-artifact-v3`. Old workload fields have been
migrated; use the recorded Git revision for historical inputs/results.

Each mix entry has a positive `weight`, a `template`, an `access` distribution,
and `compute: {min_units, max_units, prefix_fraction}`. Weights define independent
per-transaction sampling probabilities, not exact counts per block. Equal cost
bounds specify a fixed amount of CPU work. For total work `U`, prefix work is
`floor(float64(U) * prefix_fraction)` and suffix work is the remainder.

| Template | Parameters and semantics |
| --- | --- |
| `rmw` | `read_keys >= 1`, `0 <= update_keys <= read_keys`. Sample distinct reads; update the first `update_keys` using each key's own read value plus a transaction delta. Zero updates gives read-only transactions. |
| `read_write` | Distinct `read_keys` and independently sampled distinct `update_keys`; read/write sets may overlap. Writes use successive read registers, cycling if needed. |
| `state_dependent_branch` | Read a selector, choose between two sampled data keys, then write one independently sampled concrete key. |
| `selective_read_set` | `candidate_keys` fixes the candidate data set to the first K keys. Read a selector and one candidate, then write the concrete key selected by the initial selector. The selector namespace is read-only, so this isolates conservative reads versus precise static writes. |
| `staged_fan_in` | `fan_in` producers feed each join; subsequent producers read the preceding join. |
| `fan_in_fan_out` | The first `fan_in` producers feed every remaining consumer in that block. |

RMW, read/write and selector templates may share a block through `mix`. Fan-in
structures use a single template because transaction positions define the graph;
they require one data key per generated transaction. Selector keys occupy the
initialized read-only tail `[key_space, initial_keys)`. Selective and fan-in
structures assign their own keys and omit `access`.

Key distributions for RMW, read/write and two-way branch templates:

- `uniform` (also the omitted default).
- `hotspot`: `hot_keys` selects the first H keys; `hot_probability` assigns total
  probability to that set, with the remainder in the cold tail. Values 0 and 1
  are supported when the requested distinct key count fits the selected set.
- `zipf`: finite rank probabilities proportional to `rank^(-theta)` for ranks
  1 through `key_space`, with `theta` in `[0,1]`. Theta 0 is uniform. This uses a
  finite CDF; Go's `rand.NewZipf` requires an exponent greater than 1.

Multiple-key sampling is without replacement **within each read/write set**;
probabilities are conditioned on remaining keys. Reported hotspot probability
therefore describes the first draw, not a guaranteed share of distinct accesses.
RMW expresses actual read/write correlation. A mix of RMW and independent
read/write transactions replaces the old forced-reuse probability.

Linear programs execute:

```text
COMPUTE(prefix) -> READ(k1..kr) -> COMPUTE(suffix) -> WRITE(...) -> RETURN
```

Selector programs execute:

```text
READ(selector) -> COMPUTE(prefix) -> JUMP(selector) -> READ(candidate)
               -> COMPUTE(suffix) -> WRITE(concrete key) -> RETURN
```

A state read is not required to begin input-driven computation. COMPUTE currently
models deterministic CPU work; it neither transforms the selector nor produces
an address. The selector layout models work between obtaining a state parameter
and accessing the selected data, not computed dynamic addressing. Adding actual
computed addresses would require a separate runtime change.

Initial state, key/delta sampling, cost sampling and mixture sampling use separate
seeded RNG streams. With the same seed and mixture, changing cost bounds or prefix
placement preserves keys and order. Both COMPUTE instructions remain present at
zero cost so placement comparisons preserve instruction count. Split variants
keep total work, reads, writes and serial final state equal; their compute result
digests may differ. Each variant is compared with its own serial oracle.

The generator derives a sufficient gas budget from the concrete program. Actual
paths and results come from serial execution; there is no generated ground-truth
copy. Input generation remains outside the measured execution interval.

[standard-smoke.json](experiments/workload/standard-smoke.json) mixes a cheap hot
RMW, a more expensive multi-key RMW, and a selector workload. It fixes `P=8`,
`L=W`, and compares runtime, Direct-ready and estimate-abort. Locally use
`bench validate`; run performance measurements on the server. The
unit tests also cover single/multi-key, read-only, selector, fan-out and mixed
workloads at prefix fractions 0, 0.5 and 1, with `L=1/8/32/W` and `L>W`.

The design follows [transactional YCSB in Aria](https://github.com/luyi0619/aria/blob/master/benchmark/ycsb/Query.h),
[YCSB access distributions](https://github.com/brianfrankcooper/YCSB/wiki/Core-Properties),
[CHIRON's distributions and simplified contracts](https://arxiv.org/html/2401.14278v1),
and [Aptos transaction mixtures](https://github.com/aptos-labs/aptos-core/blob/main/crates/transaction-generator-lib/src/transaction_mix_generator.rs).
These are design references; this generator does not reproduce their full workloads.

## Dependency controls

Dependency acquisition, representation, and legacy scheduling use are represented by separate fields so one stage's cost cannot be hidden inside a mechanism label:

- `dependency_source ∈ {runtime_observed, static_program}` selects acquisition. `static_program` scans only engine-visible transaction programs inside the timed interval.
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

The three kernel policies change only when work happens, never what a block commits. All three fields are optional. Omitted fields resolve to `abort_and_reschedule`, `park`, and `ready_queue` for a plan with a wait consumer (`index_order` otherwise), independently of L. An explicit field is never downgraded. An explicit `ready_queue` without a wait consumer is refused because there is nothing to gate on; `suspend_yield_worker` with explicit `idle_wait_policy=gosched` is also refused. The resolved plan is written back into the case, so every run record names the behaviour that actually executed. The explicit frozen triple retains the legacy path: the original upstream entry point at `L=W`, or the legacy admission scheduler at finite L, with `kernel_policy.applied=false`. Range reads keep frozen suspend-in-place semantics, so the non-default estimate policies are unavailable to iterating transactions; the deterministic flat runtime never iterates.

Kernel policy telemetry reports what a run actually did: `estimate_suspends` / `estimate_suspend_ns`, `estimate_aborts`, `dispatch_deferrals`, `ready_queue_dispatches`, `worker_yields`, `idle_parks`, and `peak_runnable_workers`. An attempt discarded by `abort_and_reschedule` is charged the work it consumed and is reported as a replay with reason `estimate_dependency_abort`, not as a validation failure, so `validation_failures` stays specific to validation. `peak_runnable_workers` bounds how far `suspend_yield_worker` exceeded the configured executor count while replacing parked workers; a run whose peak is far above the executor count is comparing two things at once and should be read with that in mind.

Historical CQ2 records remain evidence for their recorded Git revisions and kernel policies. Older configurations with omitted kernel fields previously fell back to the frozen kernel at finite L; on the current revision they resolve to the same defaults at every L. Use the recorded revision to reproduce those results, and run the new combined matrix for current CQ2 × CQ3 comparisons. Serial cases explicitly select the frozen triple.

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

`scripts/run_workload_prefix_experiment.py` compares Runtime, Direct-ready, and Estimate-abort at `P=8`, `L=W` across five workload profiles, two compute costs, and prefix fractions `0/0.5/1`. Run it on the Linux server with eight physical cores selected from that host's topology, for example:

```sh
numactl --physcpubind=2-9 --membind=0 python3 scripts/run_workload_prefix_experiment.py results/runs/my-run --stage pilot
numactl --physcpubind=2-9 --membind=0 python3 scripts/run_workload_prefix_experiment.py results/runs/my-run --stage repeated
```

The pilot uses one seed and 1/3 warmup/measurement rounds; repeated exploration uses two different seeds and 3/30 rounds. Each stage stores its binary, generated configs, raw records, environment notes, and `summary.csv` under the results directory. Ratios below one favor the candidate over Runtime. `comparisons.csv` directly pairs Direct with Runtime and Estimate-abort, with bootstrap intervals and sign-test p-values adjusted by Holm within each comparison family and stage. The intervals themselves are unadjusted. Both stages are exploratory. `--notes` records host conditions; `--summarize-only` regenerates the CSV files without execution.

Add `--suite contention` to test early deferral with heterogeneous computation. The fixed mixture is 5% long-suffix hotspot RMW, 45% follower hotspot RMW, and 50% cold-key read-only work. Roles are sampled rather than assigned to fixed transaction positions; "head" names the long-suffix class, not a guaranteed chain head. Cold keys exclude the hotspot and cannot introduce conflicts. The suite crosses 2/4/8 hot keys with zero/1M cold compute units, holds the long-suffix class at 1M units, and adds a 100k short-head control at 4 hot keys and 1M cold units. Follower `(prefix, suffix)` costs are `(0,100k)`, `(50k,50k)`, `(90k,10k)` for fixed-total placement, and `(0,100k)`, `(100k,100k)`, `(400k,100k)` for fixed-suffix scaling. Changing costs preserves transaction roles, keys, and order within a seed and hotspot width. The pilot uses seed 71; repeated exploration uses seeds 73/7373/737373, yielding 35/105 matrices. Summary cost/prefix columns refer to followers; head and cold costs are recorded separately.

`--suite prefix-scaling` keeps that mixture at 2/4/8 hot keys, zero cold compute, and a 1M long-suffix class. It fixes follower suffix at 100k and scans prefix 0/400k/800k/1.6M/3.2M. Pilot seed 79 and repeated seeds 83/8383/838383 give 15/45 matrices, using the same 1/3 and 3/30 rounds.

`--suite speculation` fixes `P=8` and `GOMAXPROCS=8`, and crosses
`L=1/8/32/W` with Runtime, Direct-ready and Estimate-abort (12 cases per matrix).
Only L changes within each policy. The three input profiles are uniform multi-key
RMW (65,536 keys), a 99% single-key hotspot, and a Zipf mixture of single-key RMW,
multi-key RMW and read-only transactions (the latter two profiles use 1,024 keys).
Each matrix shares one 1,536-transaction block, 100k compute units per transaction
and a 0.5 prefix fraction across all cases. Pilot/repeated use the existing seeds
and round counts, producing 3/6 matrices. Ratios pair policies at the same L;
`ratio_to_lw` pairs the same policy against its full-window case. Full-window
occupancy/stall fields are blank in summaries because that telemetry is unavailable.
Run on the server after the experiment host and binding have been selected:

```sh
numactl --physcpubind=2-9 --membind=0 python3 scripts/run_workload_prefix_experiment.py results/runs/my-window-run --suite speculation --stage pilot
```

`scripts/run_hdu_experiment.py RUN_DIR --units-per-ms N --stage pilot|repeated` generates explicit, single-block artifacts with 1/8/32/128 independent motifs. Each motif contains four H (10 ms computation, write its own dependency key), four D (8 ms prefix, read the corresponding H key, 1 ms suffix, write a separate key), and four U (20 ms independent computation). HDU and HUD orders preserve transaction programs and keys; there are no group barriers. Costs are fixed CPU work, not sleeps. Calibrate N on the bound experiment CPUs with `GOMAXPROCS=8 go run scripts/hdu_timeline.go -calibrate`. Pilot covers eight fixed-cost matrices; repeated runs those anchors plus ±10% independent cost jitter at 1/128 motifs with seeds 107/10707/1070707 (20 matrices total). Fixed-cost repetitions use one input, not nominally different workload seeds. The existing 1/3 and 3/30 runner and paired analysis apply. `go run scripts/hdu_timeline.go -config MATRIX -case CASE` records separate diagnostic start/read/end/replay timestamps; diagnostic runs do not enter performance summaries.

`scripts/run_worker_experiment.py RUN_DIR --units-per-ms N --stage pilot|repeated`
compares `P=1/4/8/16/32/64` at fixed `GOMAXPROCS=8` and `L=W`. Bind the command
to eight physical CPUs and the corresponding memory node on the experiment
host, using the same binding for CPU calibration. The script inherits that
binding; changing P does not change the CPU budget. Host selection and execution
are separate from local development and validation.

Each matrix shares one workload across 18 cases: the six worker counts crossed
with Runtime, Direct-ready and Estimate-abort. Their dependency and kernel
policies come from `standard-smoke.json`; only case IDs and `executors` vary.
The existing runner interleaves cases within each round, validates against
serial execution, and starts fresh measurement processes. All cases explicitly
use `max_speculative_inflight=0` (the full block); finite L is not part of this suite.

The workload profiles are:

- Uniform multi-key RMW (4 reads, 2 updates, 65,536 keys), a low-contention CPU control.
- Single-key hotspot RMW (1 read/update, 1,024 keys, 99% hot accesses).
- HDU and HUD, each with 1 and 128 motifs (12 and 1,536 transactions).

Both synthetic profiles use one block of 1,536 transactions, 100k compute units
per transaction and a 50% prefix. Pilot uses synthetic seed 42; repeated uses
43/4243. HDU/HUD reuse the fixed-cost generator with seed 101 and zero jitter,
with identical programs and costs across order/worker comparisons. Calibrate
`N` once for the fixed CPU budget and reuse it for every P; it specifies CPU
work, not a guarantee of elapsed milliseconds at other concurrency levels.
This gives 6 pilot matrices (1/3 warmup/measurement rounds) and 8 repeated
matrices (3/30). Fixed HDU inputs are repeated measurements, not new workload samples.

`summary.csv` identifies policy and worker count, reports execution time, discarded
and reexecuted units, validation events/failures, wait counters, summed dependency
and ESTIMATE wait time, RSS, and the maximum observed per-run runnable-worker peak.
Other mechanism values are medians across measurement rounds. It also records
the observed `gomaxprocs` and allowed CPU list. `ratio_to_runtime` compares policies
at the **same P**; `ratio_to_p8` compares the **same policy** with P=8, paired by round.
Ratios below one favor the numerator. Both have exploratory, unadjusted 95%
bootstrap intervals. `comparisons.csv` retains Direct-vs-Runtime/Estimate tests
at each P; Holm adjustment includes all worker counts within each family/stage.
Wait times are sums over callbacks and can exceed block wall time; zero ESTIMATE
suspension is expected for the abort policy. `--summarize-only` rebuilds CSVs.
Local script checks: `python3 -m unittest discover -s scripts -p 'test_*.py'`.

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
