#!/usr/bin/env bash
# start-gwag.sh — start/stop the gwag gateway for perf comparison.
#
# `start` boots the example gateway from this repo on :18080 (the
# port competitors.yaml expects) and waits for /api/health.
# `stop` kills the process.
#
# Reuses bench/.run/bin/gateway if present; builds it on demand
# otherwise. PID is stashed at /tmp/perf-gwag.pid.
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
BIN="$REPO/bench/.run/bin/gateway"
PID_FILE="/tmp/perf-gwag.pid"
LOG_FILE="/tmp/perf-gwag.log"

start() {
  if [[ ! -x $BIN ]]; then
    echo "==> building gateway"
    (cd "$REPO" && go build -o "$BIN" ./examples/multi/cmd/gateway)
  fi
  mkdir -p "$REPO/bench/.run/nats/perf-gwag"
  nohup "$BIN" \
    --node-name perf-gwag \
    --http :18080 --control-plane :50090 \
    --nats-listen :14222 --nats-cluster :14248 \
    --nats-data "$REPO/bench/.run/nats/perf-gwag" \
    > "$LOG_FILE" 2>&1 &
  echo $! > "$PID_FILE"
  echo "gwag started, pid=$(cat "$PID_FILE")"
}

stop() {
  if [[ -f $PID_FILE ]]; then
    pid=$(cat "$PID_FILE")
    if kill -0 "$pid" 2>/dev/null; then
      kill "$pid" 2>/dev/null || true
      for _ in 1 2 3 4 5; do
        kill -0 "$pid" 2>/dev/null || break
        sleep 0.5
      done
      kill -9 "$pid" 2>/dev/null || true
    fi
    rm -f "$PID_FILE"
  fi
}

case "${1:-start}" in
  start) start ;;
  stop)  stop ;;
  *) echo "usage: $0 {start|stop}"; exit 1 ;;
esac
