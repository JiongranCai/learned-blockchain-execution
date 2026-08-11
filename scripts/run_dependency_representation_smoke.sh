#!/usr/bin/env bash

set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
readonly script_dir
project_root="$(cd "${script_dir}/.." && pwd)"
readonly project_root
temporary_dir="$(mktemp -d "${TMPDIR:-/tmp}/blockchain-execution-representation.XXXXXX")"
readonly temporary_dir
readonly bench_binary="${temporary_dir}/bench"

cleanup() {
  rm -rf -- "${temporary_dir}"
}
trap cleanup EXIT HUP INT TERM

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
  "configs/experiments/dependency-representation/cheap-hotspot-smoke.json"
  "configs/experiments/dependency-representation/expensive-low-conflict-smoke.json"
)

for config_file in "${configs[@]}"; do
  "${bench_binary}" validate -config "${config_file}"
  "${bench_binary}" run -config "${config_file}"
done

printf '%s\n' "dependency-representation smoke completed; raw JSONL is under results/dependency-representation/"
