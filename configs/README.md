# Experiment configuration contracts

`experiment-matrix-v1` is the single input contract for `bench validate` and `bench run`. Parsing rejects unknown fields, duplicate case IDs, unknown engines/policies, invalid trace modes, ambiguous workload sources, non-frozen workload hashes, implicit environment controls, and formal configurations that still contain placeholder or uncontrolled affinity/NUMA/page-cache values.

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
