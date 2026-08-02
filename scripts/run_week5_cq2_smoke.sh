#!/usr/bin/env bash

set -euo pipefail

readonly script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
readonly project_root="$(cd "${script_dir}/.." && pwd)"
readonly bench_binary="${TMPDIR:-/tmp}/blockchain-execution-bench-week5-cq2"

if ! command -v go >/dev/null 2>&1; then
  echo "missing required Go toolchain; no installation was attempted" >&2
  exit 1
fi

cd "${project_root}"
export GOTOOLCHAIN="${GOTOOLCHAIN:-local}"
export GOPROXY="${GOPROXY:-off}"

go test -count=1 ./...
go test -race -count=1 ./...
go vet ./...
go build -trimpath -o "${bench_binary}" ./cmd/bench

readonly configs=(
  "configs/experiments/week5-cq2/expensive-low-conflict-smoke.json"
  "configs/experiments/week5-cq2/cheap-hotspot-smoke.json"
  "configs/experiments/week5-cq2/boundary-k1-smoke.json"
  "configs/experiments/week5-cq2/boundary-k2-smoke.json"
  "configs/experiments/week5-cq2/boundary-k3-smoke.json"
)

for config in "${configs[@]}"; do
  "${bench_binary}" validate -config "${config}"
  "${bench_binary}" run -config "${config}"
done

echo "Week 5 CQ2 smoke completed; raw JSONL is under results/week5-cq2/"
