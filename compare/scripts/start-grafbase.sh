#!/usr/bin/env bash
# start-grafbase.sh — start/stop Grafbase Gateway for perf comparison.
#
# Grafbase Gateway is a Rust binary downloaded from GitHub releases
# (grafbase/grafbase, tag gateway-<version>; the project is now owned
# by The Guild). Self-hosted, static graph: no Grafbase cloud, no API
# connection. Single-subgraph mode here: it proxies queries to
# hello-graphql without doing real federation work (which our other
# backends don't support).
#
# Listens on :14300, GraphQL at /graphql. Config at
# compare/configs/grafbase/grafbase.toml + federated-graph.graphql
# (composed with grafbase CLI 0.105.2; see the file header).
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
GRAFBASE_DIR="$REPO/compare/configs/grafbase"
GRAFBASE_BIN="$REPO/compare/.run/bin/grafbase-gateway"
GRAFBASE_VERSION="0.53.5"
PID_FILE="/tmp/perf-grafbase.pid"
LOG_FILE="/tmp/perf-grafbase.log"

ensure_binary() {
  # Re-download when the cached binary is a different version, so a
  # pin bump takes effect without a manual `rm`.
  if [[ -x $GRAFBASE_BIN && "$("$GRAFBASE_BIN" --version 2>/dev/null)" == "Grafbase Gateway $GRAFBASE_VERSION" ]]; then return; fi
  mkdir -p "$(dirname "$GRAFBASE_BIN")"
  echo "==> downloading grafbase-gateway v$GRAFBASE_VERSION"
  local arch
  case "$(uname -m)" in
    x86_64) arch="x86_64-unknown-linux-musl" ;;
    aarch64) arch="aarch64-unknown-linux-musl" ;;
    *) echo "unsupported arch: $(uname -m)" >&2; exit 1 ;;
  esac
  local url="https://github.com/grafbase/grafbase/releases/download/gateway-${GRAFBASE_VERSION}/grafbase-gateway-${arch}"
  # The release asset is the bare static binary, not a tarball.
  curl -fsSL -o "$GRAFBASE_BIN" "$url"
  chmod +x "$GRAFBASE_BIN"
}

start() {
  ensure_binary
  nohup "$GRAFBASE_BIN" \
    --listen-address 0.0.0.0:14300 \
    --config "$GRAFBASE_DIR/grafbase.toml" \
    --schema "$GRAFBASE_DIR/federated-graph.graphql" \
    --log warn \
    > "$LOG_FILE" 2>&1 &
  echo $! > "$PID_FILE"
  echo "grafbase started, pid=$(cat "$PID_FILE")"
}

stop() {
  if [[ -f $PID_FILE ]]; then
    kill "$(cat "$PID_FILE")" 2>/dev/null || true
    sleep 1
    pkill -f "$GRAFBASE_BIN" 2>/dev/null || true
    rm -f "$PID_FILE"
  fi
}

case "${1:-start}" in
  start) start ;;
  stop)  stop ;;
  version) echo "$GRAFBASE_VERSION" ;;
  *) echo "usage: $0 {start|stop|version}"; exit 1 ;;
esac
