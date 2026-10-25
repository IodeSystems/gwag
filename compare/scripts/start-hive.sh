#!/usr/bin/env bash
# start-hive.sh — start/stop Hive Gateway for perf comparison.
#
# Hive Gateway (The Guild's successor to graphql-mesh v0) is the
# closest peer to gwag: multi-format ingest → one GraphQL surface.
# Mesh v1 composes a supergraph offline (`mesh-compose`), then
# `hive-gateway supergraph` serves it on :14000.
# Compose config: compare/configs/hive/mesh.config.ts
# Runtime config: compare/configs/hive/gateway.config.ts
#
# Runs a single worker (hive-gateway --fork defaults to 1; no
# clustering), same as gwag's one process per gateway here.
# First-run npm install can be slow (minutes on cold cache).
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
HIVE_DIR="$REPO/compare/configs/hive"
PID_FILE="/tmp/perf-hive.pid"
LOG_FILE="/tmp/perf-hive.log"

start() {
  if [[ ! -d $HIVE_DIR/node_modules ]]; then
    echo "==> npm install (hive gateway)"
    (cd "$HIVE_DIR" && npm install --silent --no-audit --no-fund)
  fi
  # Compose reads the live backends (introspection, OpenAPI spec) and
  # the hello.proto file, so backends must be up first. Delete
  # supergraph.graphql to force a recompose.
  if [[ ! -f $HIVE_DIR/supergraph.graphql ]]; then
    echo "==> mesh-compose"
    if ! (cd "$HIVE_DIR" && npx mesh-compose -o supergraph.graphql > "${LOG_FILE}.compose" 2>&1); then
      echo "==> mesh-compose failed; tail of ${LOG_FILE}.compose:" >&2
      tail -20 "${LOG_FILE}.compose" >&2 || true
      rm -f "$HIVE_DIR/supergraph.graphql"
      return 1
    fi
  fi
  # One worker per core (--fork). Hive's default is 1, which leaves a
  # Node gateway on one core while every other gateway here uses all
  # of them. Override with FORK=<n>.
  # setsid gives the gateway its own process group, so stop() can kill
  # the re-exec'd child and every worker in one signal. The inner bash
  # is the session leader (setsid may fork, so `$!` would be the wrong
  # pid); it records its own pid, which is the group id, then execs.
  # Run the bin directly (not via npx): one less wrapper to orphan.
  (cd "$HIVE_DIR" && FORK="${FORK:-$(nproc)}" PID_FILE="$PID_FILE" setsid nohup bash -c \
    'echo $$ > "$PID_FILE"; exec node_modules/.bin/hive-gateway supergraph supergraph.graphql --port 14000 --host 0.0.0.0 --fork "$FORK"' \
    > "$LOG_FILE" 2>&1 &)
  for _ in 1 2 3 4 5 6 7 8 9 10; do [[ -s $PID_FILE ]] && break; sleep 0.2; done
  echo "hive started, pid=$(cat "$PID_FILE")"
}

stop() {
  if [[ -f $PID_FILE ]]; then
    pid=$(cat "$PID_FILE")
    # hive-gateway re-execs itself and forks workers below that; the
    # whole tree shares the setsid process group, so signal the group
    # or a worker survives and keeps :14000 bound.
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
  version) node -p "require('$HIVE_DIR/package.json').dependencies['@graphql-hive/gateway']" ;;
  *) echo "usage: $0 {start|stop|version}"; exit 1 ;;
esac
