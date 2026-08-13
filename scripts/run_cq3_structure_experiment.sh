#!/usr/bin/env bash

set -euo pipefail

if [[ "$#" -ne 2 ]]; then
  echo "usage: $0 RUN_ID pilot|formal" >&2
  exit 2
fi

readonly run_id="$1"
readonly phase="$2"
if [[ "${phase}" != "pilot" && "${phase}" != "formal" ]]; then
  echo "phase must be pilot or formal" >&2
  exit 2
fi

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
readonly script_dir
project_root="$(cd "${script_dir}/.." && pwd)"
readonly project_root
readonly run_dir="${project_root}/results/runs/${run_id}"
readonly bench="${run_dir}/artifacts/bin/bench"

if [[ ! -x "${bench}" ]]; then
  echo "missing bench binary: ${bench}" >&2
  exit 1
fi
if ! command -v numactl >/dev/null 2>&1; then
  echo "missing numactl; no installation was attempted" >&2
  exit 1
fi

mapfile -t configs < <(find "${run_dir}/configs/${phase}" -type f -name '*.json' -print | sort)
if [[ "${#configs[@]}" -eq 0 ]]; then
  echo "no ${phase} configs under ${run_dir}" >&2
  exit 1
fi

cd "${project_root}"
for config in "${configs[@]}"; do
  printf '%s\n' "${phase^^}_VALIDATE ${config}"
  GOMAXPROCS=8 numactl --physcpubind=0-7 --membind=0 "${bench}" validate -config "${config}"
  printf '%s\n' "${phase^^}_RUN ${config}"
  GOMAXPROCS=8 numactl --physcpubind=0-7 --membind=0 "${bench}" run -config "${config}"
done
printf '%s\n' "${phase^^}_COMPLETE"
