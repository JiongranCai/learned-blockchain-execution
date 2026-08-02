# Experiment configuration contracts

`experiment-matrix-v2` is the single input contract for `bench validate` and `bench run`. Parsing rejects unknown fields, duplicate case IDs, unknown engines/policies, invalid trace modes, ambiguous workload sources, non-frozen workload hashes, implicit environment controls, negative CQ2 limits, serial limits above one, and formal configurations that still contain placeholder or uncontrolled affinity/NUMA/page-cache values.

`max_speculative_inflight` is CQ2's static admission budget. Zero means the original full-block window `W`; a positive value is reduced to `min(L,W)` for each block. The worker count remains fixed while `L` changes. A transaction occupies one slot until it enters the continuous stable validated frontier, including suspension and every incarnation; reexecution does not acquire another slot.

The performance boundary starts immediately before `Engine.ExecuteBlock` and ends immediately after it returns. State materialization and engine/policy construction happen first. Performance mode asks the engine to defer its canonical SHA-256 digest until after the interval; validation and expected-digest comparison also happen afterward. State publication remains inside the interval.

The parent runner balances case order once per round with the frozen `order_seed`. Each scheduled case is a new worker process. Warmup records are retained and marked `phase=warmup`; analyses must exclude them from measurement summaries rather than deleting them. `counters` is the formal low-cost telemetry mode; `detailed` additionally captures every action and per-decision duration for diagnosis and trace audits, so its overhead is reported separately rather than hidden in formal mechanism timings.

## StatisticalProtocol v1

`statistical/protocol-v1.json` freezes the protocol before CQ experiments:

- paired workload seeds and randomized, round-balanced case order;
- at least 3 warmup and 30 measurement rounds for a formal matrix;
- paired percentile bootstrap with 10,000 resamples and 95% confidence;
- 5% material-effect threshold and 10% median telemetry-overhead budget;
- Holm–Bonferroni correction within each preregistered comparison family;
- no discretionary successful-run outlier deletion;
- explicit censored timeout/crash/OOM records, with raw diagnostics preserved;
- at least 10,000 transactions before reporting p99;
- ranking reversal only when the paired CI excludes zero, the material threshold is exceeded, nearby workload points repeat the reversal, and switching/policy/telemetry overhead is included.

Smoke matrices may use fewer rounds, but their records remain smoke/pilot evidence and cannot be pooled with formal data.

## Week 5 CQ2 matrices

`experiments/week5-cq2/` freezes `P=8` and the four distinct effective choices `1/P/4P/W`. The two anchor matrices contrast expensive low-conflict work with a cheap single-key hotspot chain. Boundary matrices hold seed, transaction count, compute distribution, workers, and all non-CQ2 controls fixed while changing only `key_space` from 1 through 3. `linux-formal-template.json` is intentionally invalid until its target-host controls are replaced; it must not be weakened to run on macOS.
