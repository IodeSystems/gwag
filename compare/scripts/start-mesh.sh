#!/usr/bin/env bash
# start-mesh.sh — start/stop graphql-mesh for perf comparison.
#
# graphql-mesh is the closest peer to gwag (multi-format ingest →
# unified GraphQL surface). Installed via npm; serves on :14000.
# Reads its config from compare/configs/mesh/.meshrc.yaml.
#
# First-run npm install can be slow (multi-minute on cold cache).
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
MESH_DIR="$REPO/compare/configs/mesh"
PID_FILE="/tmp/perf-mesh.pid"
LOG_FILE="/tmp/perf-mesh.log"

start() {
  if [[ ! -d $MESH_DIR/node_modules ]]; then
    echo "==> npm install (graphql-mesh)"
    (cd "$MESH_DIR" && npm install --silent)
  fi
  # Modern mesh requires an explicit `build` before `start` (it used
  # to do this implicitly; current versions split the two). Build
  # writes generated SDK code into .mesh/; `start` runs it.
  if [[ ! -d $MESH_DIR/.mesh ]]; then
    echo "==> mesh build"
    if ! (cd "$MESH_DIR" && npx mesh build > "${LOG_FILE}.build" 2>&1); then
      echo "==> mesh build failed; tail of ${LOG_FILE}.build:" >&2
      tail -20 "${LOG_FILE}.build" >&2 || true
      return 1
    fi
  fi
  # setsid gives mesh its own process group so stop() reaches the node
  # children npx spawns; see start-hive.sh. A pkill by pattern missed
  # them and left mesh running through later measurements.
  (cd "$MESH_DIR" && PID_FILE="$PID_FILE" setsid nohup bash -c \
    'echo $$ > "$PID_FILE"; exec node_modules/.bin/mesh start' \
    > "$LOG_FILE" 2>&1 &)
  for _ in 1 2 3 4 5 6 7 8 9 10; do [[ -s $PID_FILE ]] && break; sleep 0.2; done
  echo "mesh started, pid=$(cat "$PID_FILE")"
}

stop() {
  if [[ -f $PID_FILE ]]; then
    pid=$(cat "$PID_FILE")
    kill -- "-$pid" 2>/dev/null || true
    for _ in 1 2 3 4 5 6 7 8 9 10; do
      kill -0 -- "-$pid" 2>/dev/null || break
      sleep 0.5
    done
    kill -9 -- "-$pid" 2>/dev/null || true
    rm -f "$PID_FILE"
  fi
}

case "${1:-start}" in
  start) start ;;
  stop)  stop ;;
  version) node -p "require('$MESH_DIR/package.json').dependencies['@graphql-mesh/cli']" ;;
  *) echo "usage: $0 {start|stop|version}"; exit 1 ;;
esac
