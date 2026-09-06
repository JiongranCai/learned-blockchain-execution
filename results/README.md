# Local Experiment Results

`results/` keeps a lightweight, Git-tracked catalogue for experiment evidence.
Complete run directories and their compressed archives stay outside Git so
large binaries and raw records do not inflate repository history. Results are
organized by immutable run rather than by a mutable experiment family path.

## Layout

```text
results/
  index.json
  archives/
    <run-id>.tar.gz       # ignored by Git
  runs/
    <run-id>/             # ignored by Git
      manifest.json
      formal/
        REPORT.md
        analysis/
        artifacts/
        <experiment-family>/<matrix>/
      pilot/
        artifacts/
        <experiment-family>/<matrix>/
```

Use this run ID format:

```text
YYYY-MM-DD-<os>-<host>-<short-commit>[-<purpose>]
```

If the same commit and host are run more than once on the same date, append a
purpose or sequence suffix. Never reuse a completed run ID.

## Keeping results

Keep pilot and formal records in separate directories under each run. Retain
configs, raw JSONL, the code revision, analysis script, and report so a result
can be reproduced. Use a new run ID for a rerun.

Large run directories and optional archives stay outside Git. Update the
lightweight `index.json` with the run location and a concise result summary.
Historical checksum entries may remain as metadata; new runs and report edits
do not require checksums, validation bundles, or numbered report copies.

To restore a downloaded archive placed in `results/archives/`:

```sh
mkdir -p results/runs
tar -xzf results/archives/<run-id>.tar.gz -C results/runs
```

The `manifest.json` inside each restored run is the entry point for automation.
The human-readable interpretation belongs in `formal/REPORT.md`.
