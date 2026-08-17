#!/usr/bin/env bash
# start-gat.sh — start/stop the embedded gat gateway for perf comparison.
#
# `start` boots compare/cmd/gat-gateway on :18085 (the port
# competitors.yaml expects) fronting the same hello-proto and
# hello-openapi backends gwag does, then waits for the GraphQL endpoint.
# `stop` kills the process.
#
# The port must NOT be gwag's :18080. The orchestrator keeps gwag up for
# the whole run, because the backends register against its control
# plane — so a gateway that tries to share the port never binds, and the
# readiness probe is answered by gwag instead. That failure is silent
# and the whole sweep then measures gwag under another gateway's name.
# mesh and apollo have always had their own ports for the same reason.
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
BIN="$REPO/bench/.run/bin/gat-gateway"
PID_FILE="/tmp/perf-gat.pid"
LOG_FILE="/tmp/perf-gat.log"

start() {
  if [[ ! -x $BIN ]]; then
    echo "==> building gat-gateway"
    (cd "$REPO" && go build -o "$BIN" ./compare/cmd/gat-gateway)
  fi
  # gat.ProtoFile resolves imports relative to cwd, so run from the
  # directory holding protos/ — same as the backends do.
  cd "$REPO/examples/multi"
  nohup "$BIN" \
    --addr :18085 --prefix /api \
    --proto protos/hello.proto --proto-target localhost:50055 \
    --openapi-url http://localhost:50053/openapi.json \
    --openapi-target http://localhost:50053 \
    > "$LOG_FILE" 2>&1 &
  echo $! > "$PID_FILE"
  # Confirm it survived startup. Without this a bind failure is silent:
  # the process dies, and whatever else holds the port answers the
  # orchestrator's readiness probe in its place.
  sleep 1
  if ! kill -0 "$(cat "$PID_FILE")" 2>/dev/null; then
    echo "gat-gateway died during startup:" >&2
    tail -5 "$LOG_FILE" >&2
    exit 1
  fi
  echo "gat started, pid=$(cat "$PID_FILE")"
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
