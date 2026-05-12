#!/usr/bin/env bash
# perf/docker-build.sh — stage the graphql-go fork into the build
# context and run `docker build`.
#
# `go.mod` has a `replace` directive pointing at the fork via a
# host-absolute path (/home/.../graphql), which isn't in the docker
# build context. This script:
#   1. Copies the fork (minus .git) into perf/.build/graphql/
#   2. Builds the image with that staging dir visible to COPY
#   3. The Dockerfile rewrites go.mod's replace directive to point
#      at /perf/.build/graphql inside the build container.
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO"

FORK_PATH="${FORK_PATH:-$(grep -E '^replace ' go.mod | head -1 | awk '{print $NF}')}"
if [[ -z $FORK_PATH || ! -d $FORK_PATH ]]; then
  echo "fatal: graphql-go fork path not found at $FORK_PATH" >&2
  echo "       set FORK_PATH=/path/to/fork or fix the replace directive in go.mod" >&2
  exit 1
fi

echo "==> staging fork from $FORK_PATH into perf/.build/graphql"
mkdir -p perf/.build
rsync -a --delete \
  --exclude='.git' --exclude='*.test' --exclude='coverage.out' --exclude='docs/' \
  "$FORK_PATH/" perf/.build/graphql/

echo "==> docker build -t gwag-perf -f perf/Dockerfile ."
exec docker build -t gwag-perf -f perf/Dockerfile "$@" .
