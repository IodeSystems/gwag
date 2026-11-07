#!/usr/bin/env bash
# Rolling-release demo: moves a provider (library) from v1 to v2 and a
# consumer (orders) from [v1] to [v2] with no downtime, watching the
# dependency graph in the admin UI.
#
#   ./demo.sh            interactive; press Enter between steps
#   ./demo.sh --check    unattended; no pauses, exits 1 on a failed check
#   DEMO_AUTO=1 ./demo.sh   unattended with DEMO_PAUSE-second sleeps
#
# Env: DEMO_PORT (gateway HTTP, 8080), DEMO_CP_PORT (control plane,
# 50090), DEMO_PAUSE (seconds between steps in auto mode, 3),
# DEMO_SUNSET_TIMEOUT (seconds to wait for v1 traffic to age out, 150).
#
# Every service is its own OS process, so the drain and SIGTERM in step
# 4 are real. Ctrl-C (or any exit) stops everything the script started.
set -uo pipefail
cd "$(dirname "$0")"
ROOT="$(cd ../.. && pwd)"

CHECK=0
for a in "$@"; do
  case "$a" in
    --check) CHECK=1 ;;
    *) echo "unknown argument: $a" >&2; exit 2 ;;
  esac
done
AUTO="${DEMO_AUTO:-0}"
[[ $CHECK == 1 ]] && AUTO=1

PORT="${DEMO_PORT:-8080}"
CP="${DEMO_CP_PORT:-50090}"
PAUSE="${DEMO_PAUSE:-3}"
SUNSET_TIMEOUT="${DEMO_SUNSET_TIMEOUT:-150}"
BASE="http://localhost:$PORT"
HOST="$(hostname -I 2>/dev/null | awk '{print $1}')"
HOST="${HOST:-localhost}"

RUN="$(mktemp -d "${TMPDIR:-/tmp}/gwag-demo.XXXXXX")"
mkdir -p "$RUN/logs"
FAILS=0
declare -A PIDS=()

kill_all() {
  local name pid
  for name in "${!PIDS[@]}"; do
    pid="${PIDS[$name]}"
    kill -TERM "$pid" 2>/dev/null || true
  done
  sleep 1
  for name in "${!PIDS[@]}"; do
    kill -KILL "${PIDS[$name]}" 2>/dev/null || true
  done
  wait 2>/dev/null || true
}
cleanup() {
  trap - EXIT INT TERM
  echo
  echo "== cleanup: stopping everything the demo started"
  kill_all
  rm -rf "$RUN"
}
trap cleanup EXIT
trap 'exit 130' INT TERM

# ---- helpers ------------------------------------------------------

say()  { printf '\n\033[1m== %s\033[0m\n' "$*"; }
look() { printf '   \033[36mlook:\033[0m %s\n' "$*"; }
ok()   { printf '   \033[32mPASS\033[0m %s\n' "$*"; }
bad()  { printf '   \033[31mFAIL\033[0m %s\n' "$*"; FAILS=$((FAILS + 1)); }

pause() {
  if [[ $AUTO == 1 ]]; then
    [[ $CHECK == 1 ]] || sleep "$PAUSE"
  else
    read -r -p "   [Enter to continue] " _ || true
  fi
}

graph() { curl -fsS "$BASE/api/admin/graph?window=${1:-1m}"; }

# until SECONDS DESC JQ_EXPR [WINDOW]: poll the graph until the jq
# expression is true. Records PASS or FAIL.
until_graph() {
  local secs="$1" desc="$2" expr="$3" win="${4:-1m}" end g
  end=$((SECONDS + secs))
  while ((SECONDS < end)); do
    g="$(graph "$win" 2>/dev/null)" || g=""
    if [[ -n "$g" ]] && jq -e "$expr" >/dev/null 2>&1 <<<"$g"; then
      ok "$desc"
      return 0
    fi
    sleep 1
  done
  bad "$desc (timeout ${secs}s; expr: $expr)"
  return 1
}

assert() { # DESC CMD...
  local desc="$1"; shift
  if "$@" >/dev/null 2>&1; then ok "$desc"; else bad "$desc"; fi
}

logs_have() { grep -q -- "$2" "$RUN/logs/$1.log"; }

# start NAME CMD...: run in the background, log to $RUN/logs/NAME.log.
start() {
  local name="$1"; shift
  "$@" >"$RUN/logs/$name.log" 2>&1 &
  PIDS[$name]=$!
  echo "   started $name (pid ${PIDS[$name]}, log $RUN/logs/$name.log)"
}

gwag() { "$RUN/gwag" "$@"; }
drain() { gwag drain "$@" --gateway "localhost:$CP"; }

# ---- build --------------------------------------------------------

say "Build"
[[ -f "$ROOT/ui/dist/index.html" ]] || {
  mkdir -p "$ROOT/ui/dist"
  cp "$ROOT/ui/fallback/index.html" "$ROOT/ui/dist/index.html"
  echo "   ui/dist empty: seeded placeholder (run bin/build for the real graph UI)"
}
for b in gateway demo-library demo-orders; do
  go build -o "$RUN/$b" "./cmd/$b" || { echo "build $b failed" >&2; exit 1; }
done
(cd "$ROOT" && go build -o "$RUN/gwag" ./gw/cmd/gwag) || { echo "build gwag failed" >&2; exit 1; }
echo "   built into $RUN"

# ---- 1. gateway ---------------------------------------------------

say "1. Gateway up"
mkdir -p "$RUN/admin"
# --public-services all: the graph and status pages show every demo
# service without signing in. Owner contacts and external dependents
# still need the dev or admin token.
start gateway "$RUN/gateway" --http ":$PORT" --control-plane ":$CP" \
  --environment demo --caller-id-public --admin-data-dir "$RUN/admin" \
  --public-services all
for _ in $(seq 1 60); do
  curl -fsS "$BASE/api/admin/graph" >/dev/null 2>&1 && break
  sleep 0.5
done
TOKEN="$(cat "$RUN/admin/admin-token" 2>/dev/null || true)"
echo "   admin token: ${TOKEN:-<not found>}"
echo "   dev token:   $(cat "$RUN/admin/dev-token" 2>/dev/null || echo '<not found>')"
echo "   gateway: $BASE   control plane: localhost:$CP"
look "open http://$HOST:$PORT/ : the graph is empty. Environment label: demo."
assert "graph endpoint answers"                      graph
until_graph 5 "environment is demo, caller ids read from header" \
  '.environment=="demo" and .callerSource=="header"'
pause

# ---- 2. consumer first --------------------------------------------

say "2. Consumer orders starts first: depends library:[v1], no provider yet"
start orders-1 "$RUN/demo-orders" --gateway "localhost:$CP" --http "$BASE" \
  --instance orders-1 --depends v1
look "orders/v1 shown as WAITING; edge orders -> library is unmet."
until_graph 20 "orders/v1 has a waiting instance" \
  '.nodes[] | select(.id=="orders/v1") | .waitingCount==1 and .readyCount==0'
until_graph 5 "edge orders -> library [v1] is unmet" \
  '.desiredEdges[] | select(.consumerNamespace=="orders" and .provider=="library") | .met==false'
sleep 1
assert "orders log says it is waiting for dependencies" logs_have orders-1 "waiting for dependencies"
pause

# ---- 3. provider v1 -----------------------------------------------

say "3. Provider library v1 (library-a) starts; orders flips READY by itself"
start library-a "$RUN/demo-library" --gateway "localhost:$CP" --instance library-a --lines v1
look "library/v1 appears; orders/v1 turns READY; edge orders -> library/v1 carries traffic."
until_graph 30 "orders/v1 is ready" \
  '.nodes[] | select(.id=="orders/v1") | .readyCount==1 and .waitingCount==0'
until_graph 20 "orders/v1 sends traffic to library/v1 (caller orders/v1)" \
  '.activeEdges[] | select(.caller=="orders/v1" and .namespace=="library" and .version=="v1") | .okCount>0 and .consumerNamespace=="orders" and .consumerVersion=="v1"'
until_graph 5 "library/v1 is not sunset-able (orders accepts only v1)" \
  '.nodes[] | select(.id=="library/v1") | .sunsetable==false'
pause

# ---- 4. blue/green ------------------------------------------------

say "4. Blue/green: library-b serves v1 and v2 from one Register; drain library-a"
start library-b "$RUN/demo-library" --gateway "localhost:$CP" --instance library-b --lines v1,v2
look "library/v2 appears. library/v1 has two replicas (library-a, library-b)."
until_graph 30 "library/v1 has 2 ready instances, library/v2 has 1" \
  '([.nodes[] | select(.id=="library/v1") | .readyCount][0]==2) and ([.nodes[] | select(.id=="library/v2") | .readyCount][0]==1)'
echo
echo "   \$ gwag drain library-a --wait"
if out="$(drain library-a --wait --timeout 60s 2>&1)"; then
  echo "$out" | sed 's/^/     /'
  if grep -q safe_to_stop <<<"$out"; then ok "library-a reaches safe_to_stop (library-b covers v1)"; else bad "library-a: no safe_to_stop in output"; fi
else
  echo "$out" | sed 's/^/     /'
  bad "gwag drain library-a --wait failed"
fi
look "library-a shows draining -> safe_to_stop in the graph; orders stays READY."
echo "   \$ kill -TERM library-a   (DrainOnSIGTERM: already safe_to_stop, deregisters, exits)"
kill -TERM "${PIDS[library-a]}"
if timeout 20 tail --pid="${PIDS[library-a]}" -f /dev/null 2>/dev/null; then
  ok "library-a exited"
else
  bad "library-a still running 20s after SIGTERM"
fi
unset 'PIDS[library-a]'
assert "library-a logged a clean drain" logs_have library-a "drained and deregistered"
until_graph 15 "library/v1 has 1 instance left (library-b); orders still ready" \
  '([.nodes[] | select(.id=="library/v1") | .replicaCount][0]==1) and ([.nodes[] | select(.id=="orders/v1") | .readyCount][0]==1)'
pause

# ---- 5. consumer moves v1 -> [v1,v2] -> [v2] ----------------------

restart_orders() { # NAME OLD DEPENDS
  local name="$1" old="$2" deps="$3"
  echo "   \$ kill -TERM $old ; start $name --depends $deps"
  kill -TERM "${PIDS[$old]}"
  timeout 20 tail --pid="${PIDS[$old]}" -f /dev/null 2>/dev/null || bad "$old did not exit after SIGTERM"
  unset "PIDS[$old]"
  start "$name" "$RUN/demo-orders" --gateway "localhost:$CP" --http "$BASE" \
    --instance "$name" --depends "$deps"
}

say "5a. Consumer release: orders accepts [v1, v2]; it calls v2 when present"
restart_orders orders-2 orders-1 v1,v2
look "orders edge now reads library [v1, v2]; the active edge moves to library/v2."
until_graph 30 "orders/v1 ready with depends [v1, v2]" \
  '.nodes[] | select(.id=="orders/v1") | .readyCount==1'
until_graph 5 "desired edge orders -> library lines [v1,v2], both present" \
  '.desiredEdges[] | select(.consumerNamespace=="orders" and .provider=="library") | (.lines|sort)==["v1","v2"] and (.presentLines|sort)==["v1","v2"]'
until_graph 20 "orders now sends successful traffic to library/v2" \
  '.activeEdges[] | select(.caller=="orders/v1" and .namespace=="library" and .version=="v2") | .okCount>0'
assert "orders log: now calling library v2" logs_have orders-2 "now calling library v2"
pause

say "5b. Consumer release: orders accepts [v2] only"
restart_orders orders-3 orders-2 v2
look "edge orders -> library reads [v2]. library/v1 has no dependents left."
until_graph 30 "desired edge orders -> library lines [v2]" \
  '.desiredEdges[] | select(.consumerNamespace=="orders" and .provider=="library") | .lines==["v2"] and .met==true'
pause

# ---- 6. sunset ----------------------------------------------------

say "6. library v1 becomes sunset-able once its traffic ages out of the 1m window"
look "click library/v1: sunset-able badge appears once no call landed on v1 for 60s (window 1m)."
echo "   waiting up to ${SUNSET_TIMEOUT}s (expect ~60-70s after the last v1 call)..."
t0=$SECONDS
if until_graph "$SUNSET_TIMEOUT" "library/v1 is sunset-able (window 1m)" \
  '.nodes[] | select(.id=="library/v1") | .sunsetable==true and .trafficCount==0'; then
  echo "   sunset-able after $((SECONDS - t0))s of waiting in this step"
fi
until_graph 5 "library/v2 is not sunset-able (orders depends on it)" \
  '.nodes[] | select(.id=="library/v2") | .sunsetable==false'
echo
echo "   Now a drain that would strand a dependent: library-b is the only v2 instance."
echo "   \$ gwag drain library-b"
out="$(drain library-b 2>&1)"; echo "$out" | sed 's/^/     /'
if grep -q blocked <<<"$out" && grep -q "blocked:" <<<"$out"; then ok "drain library-b is blocked, with reasons"; else bad "drain library-b: expected blocked + reasons"; fi
look "library-b shows 'blocked' with the reason (orders would have no ready library line)."
until_graph 5 "graph shows library-b blocked" \
  '[.nodes[].instances[] | select(.instanceId=="library-b") | .drainState][0]=="blocked"'
pause
echo "   \$ gwag drain library-b --cancel"
out="$(drain library-b --cancel 2>&1)"; echo "$out" | sed 's/^/     /'
until_graph 10 "library-b serves again after --cancel" \
  '[.nodes[].instances[] | select(.instanceId=="library-b") | .drainState][0]=="serving"'
pause

# ---- 7. done ------------------------------------------------------

say "Done"
if ((FAILS > 0)); then
  echo "   $FAILS check(s) failed. Logs: $RUN/logs (removed on exit; set KEEP_LOGS=1 to copy)"
  [[ -n "${KEEP_LOGS:-}" ]] && cp -r "$RUN/logs" ./demo-logs
  [[ $CHECK == 1 ]] && exit 1
else
  echo "   all checks passed"
fi
exit 0
