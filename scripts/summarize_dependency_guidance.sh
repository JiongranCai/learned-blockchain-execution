#!/usr/bin/env bash

set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
readonly script_dir
project_root="$(cd "${script_dir}/.." && pwd)"
readonly project_root
readonly results_root="${1:-${project_root}/results/dependency-guidance}"

if ! command -v jq >/dev/null 2>&1; then
  printf '%s\n' "missing required jq executable; no installation was attempted" >&2
  exit 1
fi

shopt -s nullglob
readonly run_files=("${results_root}"/*/runs.jsonl)
if [[ ${#run_files[@]} -eq 0 ]]; then
  printf '%s\n' "no dependency-guidance run files found under ${results_root}" >&2
  exit 1
fi

printf '%s\n' $'matrix\tcase\tmode\tsource\tn\tmedian_ms\treexecuted_units\tacquisition_us\trepresentation_us\tresolution_worker_us\twait_worker_us\tedges\tsummary_entries'

for run_file in "${run_files[@]}"; do
  matrix="$(basename "$(dirname "${run_file}")")"
  jq -rs --arg matrix "${matrix}" '
    def median:
      sort as $sorted
      | ($sorted | length) as $count
      | if $count % 2 == 1
        then $sorted[($count / 2) | floor]
        else (($sorted[$count / 2 - 1] + $sorted[$count / 2]) / 2)
        end;
    map(select(.phase == "measurement" and .status == "success" and .canonical_match))
    | group_by(.case.id)
    | .[]
    | [
        $matrix,
        .[0].case.id,
        .[0].case.dependency_mode,
        .[0].case.dependency_source,
        length,
        ((map(.timing.execution_ns) | median) / 1000000),
        (map(.metrics.reexecuted_execution_units) | median),
        ((map(.metrics.dependency.acquisition_ns) | median) / 1000),
        ((map(.metrics.dependency.representation_ns) | median) / 1000),
        ((map(.metrics.dependency.resolution_ns) | median) / 1000),
        ((map(.metrics.dependency.wait_ns) | median) / 1000),
        (map(.metrics.dependency.dependency_edges) | median),
        (map(.metrics.dependency.summary_entries) | median)
      ]
    | @tsv
  ' "${run_file}"
done
