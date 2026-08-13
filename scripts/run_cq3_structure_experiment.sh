#!/usr/bin/env bash

set -euo pipefail

if [[ "$#" -lt 2 || "$#" -gt 3 ]]; then
  echo "usage: $0 RUN_ID pilot|formal [--resume]" >&2
  exit 2
fi

readonly run_id="$1"
readonly phase="$2"
if [[ "${phase}" != "pilot" && "${phase}" != "formal" ]]; then
  echo "phase must be pilot or formal" >&2
  exit 2
fi
readonly resume="${3:-}"
if [[ -n "${resume}" && "${resume}" != "--resume" ]]; then
  echo "third argument must be --resume" >&2
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
  run_records="$(python3 -c 'import json, sys; print(json.load(open(sys.argv[1], encoding="utf-8"))["output"]["run_records"])' "${config}")"
  expected_records="$(python3 -c 'import json, sys; value=json.load(open(sys.argv[1], encoding="utf-8")); print(len(value["cases"]) * (value["warmup_rounds"] + value["measurement_rounds"]))' "${config}")"
  if [[ "${resume}" == "--resume" && -f "${run_records}" ]]; then
    actual_records="$(wc -l < "${run_records}")"
    if [[ "${actual_records}" -ne "${expected_records}" ]]; then
      echo "refusing to append to partial matrix: ${run_records} (${actual_records}/${expected_records} records)" >&2
      exit 1
    fi
    if grep -Eq '"status":"(error|timeout)"|"censored":true|"canonical_match":false' "${run_records}"; then
      echo "refusing to skip unsuccessful matrix: ${run_records}" >&2
      exit 1
    fi
    printf '%s\n' "${phase^^}_SKIP_COMPLETE ${config}"
    continue
  fi
  printf '%s\n' "${phase^^}_VALIDATE ${config}"
  GOMAXPROCS=8 numactl --physcpubind=0-7 --membind=0 "${bench}" validate -config "${config}"
  printf '%s\n' "${phase^^}_RUN ${config}"
  GOMAXPROCS=8 numactl --physcpubind=0-7 --membind=0 "${bench}" run -config "${config}"
  actual_records="$(wc -l < "${run_records}")"
  if [[ "${actual_records}" -ne "${expected_records}" ]]; then
    echo "matrix record count mismatch: ${run_records} (${actual_records}/${expected_records})" >&2
    exit 1
  fi
done
printf '%s\n' "${phase^^}_COMPLETE"
