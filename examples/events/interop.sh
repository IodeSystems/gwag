#!/usr/bin/env bash
# Go <-> TypeScript interop over one NATS JetStream server.
#
#   examples/events/interop.sh
#
# Phases:
#   0. The TypeScript example (ts/src/main.ts) publishes and handles its own events.
#   1. Go publishes the sample events, a TypeScript listener receives them.
#   2. TypeScript publishes, a Go listener receives them.
#   3. A Go and a TypeScript listener share one group; both publish, and
#      every message reaches exactly one of the two.
#
# NATS comes from docker (nats:2.10-alpine, host port NATS_PORT, default
# 14224). To use your own server, set NATS_URL to one with JetStream on.
# Needs go, node >= 24 and pnpm; run `pnpm install` in ts/events and
# examples/events/ts first (check.sh does).
set -euo pipefail
cd "$(dirname "$0")"
HERE="$(pwd)"
ROOT="$(cd ../.. && pwd)"
export LC_ALL=C

NATS_IMAGE="${NATS_IMAGE:-nats:2.10-alpine}"
NATS_PORT="${NATS_PORT:-14224}"
WORK="$(mktemp -d)"
NAME="gwag-events-interop-$$"
pids=()

cleanup() {
  for p in "${pids[@]:-}"; do [[ -n "$p" ]] && kill "$p" 2>/dev/null || true; done
  docker rm -f "$NAME" >/dev/null 2>&1 || true
  rm -rf "$WORK"
}
trap cleanup EXIT INT TERM

if [[ -z "${NATS_URL:-}" ]]; then
  docker run -d --name "$NAME" -p "127.0.0.1:$NATS_PORT:4222" "$NATS_IMAGE" -js >/dev/null
  for _ in $(seq 100); do
    (exec 3<>"/dev/tcp/127.0.0.1/$NATS_PORT") 2>/dev/null && break
    sleep 0.1
  done
  sleep 0.5
  export NATS_URL="nats://127.0.0.1:$NATS_PORT"
fi
echo "== NATS at $NATS_URL"

(cd "$ROOT" && go build -o "$WORK/go-interop" ./examples/events/go-interop)
GO="$WORK/go-interop"
TS=(node "$HERE/ts/src/interop.ts")

wait_ready() { # file
  for _ in $(seq 300); do
    grep -qx ready "$1" 2>/dev/null && return 0
    sleep 0.1
  done
  echo "FAIL listener never became ready ($1)" >&2
  cat "$1" >&2
  return 1
}

cat >"$WORK/want" <<'EOF'
Audit o-1002.refund: support refund
OrderEvents o-1001 created by ann
OrderEvents o-1002 cancelled: out of stock
OrderShipped o-1001 via ups
Payments eu/card captured 4200 for o-1001
Payments eu/card refunded 1500: cancelled
Payments us/wire note hello
EOF

fail=0
check_listener() { # label, file, pid
  local label="$1" file="$2" pid="$3"
  if ! wait "$pid"; then echo "FAIL $label listener exited non-zero" >&2; cat "$file" >&2; fail=1; return; fi
  grep -vx ready "$file" | sort >"$file.got"
  if diff -u "$WORK/want" "$file.got"; then echo "ok   $label"; else echo "FAIL $label" >&2; fail=1; fi
}

echo "== 0. TypeScript example over JetStream"
out="$(node "$HERE/ts/src/main.ts")"
echo "$out"
for want in \
  "subscribes    orders.v1.OrderEvents as billing" \
  "OrderEvents   o-1001 created by ann" \
  "OrderEvents   o-1002 cancelled: out of stock" \
  "Payments      eu/card captured 4200 for o-1001" \
  "Payments      eu/card refunded 1500: cancelled" \
  "OrderShipped  o-1001 via ups" \
  "Audit         o-1002.refund: support refund"; do
  if grep -qF -- "$want" <<<"$out"; then echo "ok   $want"; else echo "FAIL missing '$want'" >&2; fail=1; fi
done

echo "== 1. Go publishes, TypeScript listens"
"${TS[@]}" listen >"$WORK/p1" & pid=$!; pids+=("$pid")
wait_ready "$WORK/p1"
"$GO" publish
check_listener "go -> ts" "$WORK/p1" "$pid"

echo "== 2. TypeScript publishes, Go listens"
"$GO" listen >"$WORK/p2" & pid=$!; pids+=("$pid")
wait_ready "$WORK/p2"
"${TS[@]}" publish
check_listener "ts -> go" "$WORK/p2" "$pid"

echo "== 3. one group, a Go and a TypeScript member"
N=20
"$GO" listen-shared >"$WORK/gs" & gpid=$!; pids+=("$gpid")
"${TS[@]}" listen-shared >"$WORK/ts" & tpid=$!; pids+=("$tpid")
wait_ready "$WORK/gs"
wait_ready "$WORK/ts"
"$GO" publish-many "$N"
"${TS[@]}" publish-many "$N"
total=0
for _ in $(seq 300); do
  total=$(cat "$WORK/gs" "$WORK/ts" | grep -c '^shared ' || true)
  [[ "$total" -ge $((2 * N)) ]] && break
  sleep 0.1
done
sleep 1 # let a duplicate delivery show up
kill "$gpid" "$tpid" 2>/dev/null || true
wait "$gpid" "$tpid" 2>/dev/null || true
cat "$WORK/gs" "$WORK/ts" | grep '^shared ' | sort >"$WORK/all"
ngo=$(grep -c '^shared ' "$WORK/gs" || true)
nts=$(grep -c '^shared ' "$WORK/ts" || true)
uniq=$(sort -u "$WORK/all" | wc -l)
echo "go member got $ngo, ts member got $nts, $uniq distinct of $((2 * N))"
if [[ "$ngo" -gt 0 && "$nts" -gt 0 && $((ngo + nts)) -eq $((2 * N)) && "$uniq" -eq $((2 * N)) ]]; then
  echo "ok   shared group"
else
  echo "FAIL shared group" >&2
  fail=1
fi

if [[ $fail == 0 ]]; then echo "PASS"; else echo "FAILED" >&2; exit 1; fi
