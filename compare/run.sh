#!/usr/bin/env bash
# compare/run.sh — entrypoint for both docker and host-local runs.
#
#   compare/run.sh local              # use the host's go + node toolchains
#   compare/run.sh                    # default: assume in-container, already provisioned
#   compare/run.sh local --only gwag  # restrict to one gateway
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
if [[ ! -x compare/.run/bin/compare || compare/cmd/compare/main.go -nt compare/.run/bin/compare ]]; then
  echo "==> building compare orchestrator"
  mkdir -p compare/.run/bin
  go build -o compare/.run/bin/compare ./compare/cmd/compare
fi

mkdir -p compare/.out
compare/.run/bin/compare \
  --config compare/competitors.yaml \
  --out compare/.out \
  --repo "$REPO" \
  "${EXTRA_ARGS[@]}"

# Mirror the rendered comparison.md to the tracked path so the
# published artefact lives in git. Intermediate JSONs stay under
# compare/.out/ (gitignored).
if [[ -f compare/.out/comparison.md ]]; then
  cp compare/.out/comparison.md compare/comparison.md
fi
