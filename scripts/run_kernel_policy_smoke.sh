#!/usr/bin/env bash

set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
readonly script_dir
project_root="$(cd "${script_dir}/.." && pwd)"
readonly project_root
temporary_dir="$(mktemp -d "${TMPDIR:-/tmp}/blockchain-execution-kernel-policy.XXXXXX")"
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

go build -trimpath -o "${bench_binary}" ./cmd/bench

readonly config_file="configs/experiments/kernel-policy/selective-read-set-smoke.json"

"${bench_binary}" run -config "${config_file}"

printf '%s\n' "kernel-policy smoke completed; raw JSONL is under results/kernel-policy/"
