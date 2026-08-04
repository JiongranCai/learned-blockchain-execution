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
    SHA256SUMS
    <run-id>.tar.gz       # ignored by Git
  runs/
    <run-id>/             # ignored by Git
      manifest.json
      MIGRATION-SHA256SUMS
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

## Rules

1. Create the run directory before generating configs.
2. Point every config output at
   `results/runs/<run-id>/<run-class>/<family>/<matrix>/`.
3. Keep pilot and formal records in separate run-class directories.
4. Treat raw JSONL, validation bundles, configs, and binaries as immutable.
   A report correction must preserve the previous report as a numbered version
   and refresh the run manifest and checksum. A rerun gets a new run ID.
5. Retain the exact configs, VCS-stamped binary, hashes, provenance, validation
   bundles, raw JSONL, analysis program, and final report.
6. Create `results/archives/<run-id>.tar.gz` with the run directory as its
   single top-level entry.
7. Record the archive path, byte size, and SHA-256 in `index.json`, and add its
   checksum to `results/archives/SHA256SUMS`.
8. Commit only the lightweight catalogue files. Both `runs/<run-id>/` and the
   compressed archive are ignored by Git.
9. Transfer the archive separately and keep at least one additional backup.

To restore a downloaded archive placed in `results/archives/`:

```sh
sha256sum -c results/archives/SHA256SUMS
mkdir -p results/runs
tar -xzf results/archives/<run-id>.tar.gz -C results/runs
```

The `manifest.json` inside each restored run is the entry point for automation.
The human-readable interpretation belongs in `formal/REPORT.md`.
