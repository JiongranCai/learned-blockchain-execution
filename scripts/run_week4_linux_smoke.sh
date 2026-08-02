#!/bin/sh

set -eu

if [ "$(uname -s)" != "Linux" ]; then
	printf '%s\n' "run_week4_linux_smoke.sh requires a Linux host" >&2
	exit 1
fi

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
project_root=$(CDPATH= cd -- "${script_dir}/.." && pwd)
temporary_dir=$(mktemp -d "${TMPDIR:-/tmp}/blockchain-execution-week4.XXXXXX")

cleanup() {
	rm -rf -- "${temporary_dir}"
}
trap cleanup EXIT HUP INT TERM

cd "${project_root}"

git status --short --branch
go version
uname -srm

GOPROXY=off go test -count=1 ./...
GOPROXY=off go test -race -count=1 ./...
GOPROXY=off go vet ./...
GOPROXY=off go build -trimpath -o "${temporary_dir}/bench" ./cmd/bench

"${temporary_dir}/bench" validate -config configs/experiments/week4-smoke.json
"${temporary_dir}/bench" run -config configs/experiments/week4-smoke.json

printf '%s\n' "Week 4 Linux smoke passed; generated records are under results/week4-smoke"
