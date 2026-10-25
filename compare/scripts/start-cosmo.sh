#!/usr/bin/env bash
# start-cosmo.sh — start/stop WunderGraph Cosmo Router for perf comparison.
#
# Cosmo Router is a Go binary downloaded from GitHub releases. It runs
# WITHOUT the Cosmo control plane: the execution config is composed
# offline from the subgraph SDL with `wgc router compose` and committed
# (compare/configs/cosmo/execution-config.json), so a run needs no npm.
# Single-subgraph mode: proxies queries to hello-graphql.
#
# Listens on :14200, path /graphql. Config at
# compare/configs/cosmo/config.yaml. Recompose (wgc 0.133.3) with the
# command in compare/configs/cosmo/graph.yaml.
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
COSMO_DIR="$REPO/compare/configs/cosmo"
# Not named "router": that is Apollo's binary name in the same dir.
COSMO_BIN="$REPO/compare/.run/bin/cosmo-router"
COSMO_VERSION="0.363.0"
PID_FILE="/tmp/perf-cosmo.pid"
LOG_FILE="/tmp/perf-cosmo.log"

ensure_binary() {
  # Re-download when the cached binary is a different version, so a
  # pin bump takes effect without a manual `rm`.
  if [[ -x $COSMO_BIN ]] && "$COSMO_BIN" --version 2>/dev/null | grep -qx "  Version: $COSMO_VERSION"; then return; fi
  mkdir -p "$(dirname "$COSMO_BIN")"
  echo "==> downloading cosmo router v$COSMO_VERSION"
  local arch
  case "$(uname -m)" in
    x86_64) arch="linux-amd64" ;;
    aarch64) arch="linux-arm64" ;;
    *) echo "unsupported arch: $(uname -m)" >&2; exit 1 ;;
  esac
  local url="https://github.com/wundergraph/cosmo/releases/download/router%40${COSMO_VERSION}/router-router%40${COSMO_VERSION}-${arch}.tar.gz"
  # The tarball holds `router` + LICENSE at top level. Extract into a
  # temp dir so we don't clobber Apollo's .run/bin/router.
  local tmp
  tmp="$(mktemp -d)"
  curl -fsSL "$url" | tar -xz -C "$tmp" router
  mv "$tmp/router" "$COSMO_BIN"
  rmdir "$tmp"
  chmod +x "$COSMO_BIN"
}

start() {
  ensure_binary
  # cwd = config dir: config.yaml's execution_config path is relative.
  cd "$COSMO_DIR"
  nohup "$COSMO_BIN" --config config.yaml > "$LOG_FILE" 2>&1 &
  echo $! > "$PID_FILE"
  echo "cosmo started, pid=$(cat "$PID_FILE")"
}

stop() {
  if [[ -f $PID_FILE ]]; then
    kill "$(cat "$PID_FILE")" 2>/dev/null || true
    sleep 1
    pkill -f "$COSMO_BIN" 2>/dev/null || true
    rm -f "$PID_FILE"
  fi
}

case "${1:-start}" in
  start) start ;;
  stop)  stop ;;
  version) echo "$COSMO_VERSION" ;;
  *) echo "usage: $0 {start|stop|version}"; exit 1 ;;
esac
