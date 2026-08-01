#!/usr/bin/env bash

set -euo pipefail

readonly expected_upstream_commit="7afe924fb4a611a2626f92338f1f76e4ebefa62f"
readonly script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
readonly project_root="$(cd "${script_dir}/.." && pwd)"

cd "${project_root}"

if ! git cat-file -e "${expected_upstream_commit}^{commit}"; then
  echo "missing frozen upstream commit: ${expected_upstream_commit}" >&2
  exit 1
fi

if ! git merge-base --is-ancestor "${expected_upstream_commit}" HEAD; then
  echo "HEAD does not contain the frozen upstream commit" >&2
  exit 1
fi

if ! git diff --quiet "${expected_upstream_commit}" -- \
  '*.go' go.mod go.sum LICENSE .github/workflows/go.yml; then
  echo "upstream kernel differs from the frozen baseline" >&2
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

go mod verify
go build ./...
go test -count=1 ./...
go test -race -count=1 ./...
go vet ./...

for workers in 1 2 8; do
  GOMAXPROCS="${workers}" go test -run '^TestSTM$' -count=10 ./...
done

go test -run '^$' \
  -bench '^BenchmarkBlockSTM/no-conflict-10000-(sequential|worker-5)$' \
  -benchtime=1x -count=1 -benchmem ./...

echo "baseline verification passed"
