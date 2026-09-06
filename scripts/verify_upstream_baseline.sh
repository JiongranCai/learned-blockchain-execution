#!/usr/bin/env bash

set -euo pipefail

readonly expected_upstream_commit="7afe924fb4a611a2626f92338f1f76e4ebefa62f"
script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
readonly script_dir
project_root="$(cd "${script_dir}/.." && pwd)"
readonly project_root

cd "${project_root}"

if ! git cat-file -e "${expected_upstream_commit}^{commit}"; then
  echo "missing frozen upstream commit: ${expected_upstream_commit}" >&2
  exit 1
fi

if ! git merge-base --is-ancestor "${expected_upstream_commit}" HEAD; then
  echo "HEAD does not contain the frozen upstream commit" >&2
  exit 1
fi

if ! git diff --quiet --diff-filter=MD "${expected_upstream_commit}" -- \
  ':(top,glob)*.go' go.mod go.sum LICENSE; then
  echo "an original upstream kernel file differs from the frozen baseline" >&2
  exit 1
fi

if [[ -n "${BASELINE_CACHE_ROOT:-}" ]]; then
  export GOCACHE="${BASELINE_CACHE_ROOT}/build"
  export GOMODCACHE="${BASELINE_CACHE_ROOT}/mod"
fi

export GOTOOLCHAIN="${GOTOOLCHAIN:-local}"
if [[ "${OFFLINE:-0}" == "1" ]]; then
  export GOPROXY=off
fi

go test -count=1 ./...
go test -race -count=1 ./...
go vet ./...

echo "baseline verification passed"
