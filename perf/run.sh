#!/usr/bin/env bash
# perf/run.sh — entrypoint for both docker and host-local runs.
#
#   perf/run.sh local              # use the host's go + node toolchains
#   perf/run.sh                    # default: assume in-container, already provisioned
#   perf/run.sh local --only gwag  # restrict to one gateway
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO"

MODE="docker"
if [[ "${1:-}" == "local" ]]; then
  MODE="local"
  shift
fi

EXTRA_ARGS=("$@")

# Build the comparator binary on first run / when source changed.
if [[ ! -x perf/.run/bin/compare || perf/cmd/compare/main.go -nt perf/.run/bin/compare ]]; then
  echo "==> building compare orchestrator"
  mkdir -p perf/.run/bin
  go build -o perf/.run/bin/compare ./perf/cmd/compare
fi

mkdir -p perf/.out
exec perf/.run/bin/compare \
  --config perf/competitors.yaml \
  --out perf/.out \
  --repo "$REPO" \
  "${EXTRA_ARGS[@]}"
