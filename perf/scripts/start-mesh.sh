#!/usr/bin/env bash
# start-mesh.sh — start/stop graphql-mesh for perf comparison.
#
# graphql-mesh is the closest peer to gwag (multi-format ingest →
# unified GraphQL surface). Installed via npm; serves on :14000.
# Reads its config from perf/configs/mesh/.meshrc.yaml.
#
# First-run npm install can be slow (multi-minute on cold cache).
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
MESH_DIR="$REPO/perf/configs/mesh"
PID_FILE="/tmp/perf-mesh.pid"
LOG_FILE="/tmp/perf-mesh.log"

start() {
  if [[ ! -d $MESH_DIR/node_modules ]]; then
    echo "==> npm install (graphql-mesh)"
    (cd "$MESH_DIR" && npm install --silent)
  fi
  (cd "$MESH_DIR" && nohup npx mesh start > "$LOG_FILE" 2>&1 & echo $! > "$PID_FILE")
  echo "mesh started, pid=$(cat "$PID_FILE")"
}

stop() {
  if [[ -f $PID_FILE ]]; then
    pid=$(cat "$PID_FILE")
    # mesh forks a child node process — pkill the whole process group.
    pkill -P "$pid" 2>/dev/null || true
    kill "$pid" 2>/dev/null || true
    sleep 1
    pkill -f "node.*mesh" 2>/dev/null || true
    rm -f "$PID_FILE"
  fi
}

case "${1:-start}" in
  start) start ;;
  stop)  stop ;;
  *) echo "usage: $0 {start|stop}"; exit 1 ;;
esac
