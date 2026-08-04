#!/usr/bin/env bash

set -euo pipefail

if [[ "$(uname -s)" != "Linux" ]]; then
  printf '%s\n' "run_baseline_linux_smoke.sh requires a Linux host" >&2
  exit 1
fi

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
readonly script_dir
project_root="$(cd "${script_dir}/.." && pwd)"
readonly project_root
temporary_dir="$(mktemp -d "${TMPDIR:-/tmp}/blockchain-execution-baseline.XXXXXX")"
readonly temporary_dir

cleanup() {
  rm -rf -- "${temporary_dir}"
}
trap cleanup EXIT HUP INT TERM

cd "${project_root}"
export GOTOOLCHAIN="${GOTOOLCHAIN:-local}"
export GOPROXY="${GOPROXY:-off}"

git status --short --branch
go version
uname -srm

go test -count=1 ./...
go test -race -count=1 ./...
go vet ./...
go build -trimpath -o "${temporary_dir}/bench" ./cmd/bench

"${temporary_dir}/bench" validate -config configs/experiments/baseline/smoke.json
"${temporary_dir}/bench" run -config configs/experiments/baseline/smoke.json

printf '%s\n' "baseline Linux smoke passed; generated records are under results/baseline/smoke"
