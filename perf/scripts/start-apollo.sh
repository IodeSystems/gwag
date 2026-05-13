#!/usr/bin/env bash
# start-apollo.sh — start/stop Apollo Router for perf comparison.
#
# Apollo Router is a Rust binary downloaded from GitHub releases.
# Single-subgraph mode here: it proxies queries to hello-graphql
# without doing real federation work (which our other backends
# don't support).
#
# Listens on :14100. Config at perf/configs/apollo/router.yaml +
# perf/configs/apollo/supergraph.graphql.
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
APOLLO_DIR="$REPO/perf/configs/apollo"
APOLLO_BIN="$REPO/perf/.run/bin/router"
APOLLO_VERSION="1.55.0"
PID_FILE="/tmp/perf-apollo.pid"
LOG_FILE="/tmp/perf-apollo.log"

ensure_binary() {
  if [[ -x $APOLLO_BIN ]]; then return; fi
  mkdir -p "$(dirname "$APOLLO_BIN")"
  echo "==> downloading apollo-router v$APOLLO_VERSION"
  local arch
  case "$(uname -m)" in
    x86_64) arch="x86_64-unknown-linux-gnu" ;;
    aarch64) arch="aarch64-unknown-linux-gnu" ;;
    *) echo "unsupported arch: $(uname -m)" >&2; exit 1 ;;
  esac
  local url="https://github.com/apollographql/router/releases/download/v${APOLLO_VERSION}/router-v${APOLLO_VERSION}-${arch}.tar.gz"
  # The tarball lays out as dist/{router,LICENSE,...};
  # --strip-components=1 drops the dist/ prefix so the binary
  # lands directly at $APOLLO_BIN.
  curl -fsSL "$url" | tar -xz --strip-components=1 -C "$(dirname "$APOLLO_BIN")" dist/router
  chmod +x "$APOLLO_BIN"
}

start() {
  ensure_binary
  nohup "$APOLLO_BIN" \
    --config "$APOLLO_DIR/router.yaml" \
    --supergraph "$APOLLO_DIR/supergraph.graphql" \
    > "$LOG_FILE" 2>&1 &
  echo $! > "$PID_FILE"
  echo "apollo started, pid=$(cat "$PID_FILE")"
}

stop() {
  if [[ -f $PID_FILE ]]; then
    kill "$(cat "$PID_FILE")" 2>/dev/null || true
    sleep 1
    pkill -f "$APOLLO_BIN" 2>/dev/null || true
    rm -f "$PID_FILE"
  fi
}

case "${1:-start}" in
  start) start ;;
  stop)  stop ;;
  *) echo "usage: $0 {start|stop}"; exit 1 ;;
esac
