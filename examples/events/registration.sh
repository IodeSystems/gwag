#!/usr/bin/env bash
# The TypeScript and Python registration clients against a real gateway.
#
#   examples/events/registration.sh
#
# Builds gw/cmd/gwag, boots `gwag up --schema-registry` in a temp dir
# and checks:
#   1. the Schema Registry serves control.proto as the default subject
#      gateway.controlplane.v1.ControlPlane, and the generators the two
#      client packages pin turn that fetched file into client code;
#   2. a Python service that subscribes to a channel nobody declares
#      yet waits for the gateway's ok (it does not start);
#   3. a TypeScript service that declares the channels (orders)
#      registers, and the waiting Python service becomes ready;
#   4. a TypeScript service subscribing to the same channel registers;
#   5. `gwag ctl services list` shows all three, and the admin API shows
#      each listener's Subscribes;
#   6. SIGTERM deregisters both listeners at once (not after the TTL).
#
# Needs go, node >= 24 with pnpm, and uv. No docker.
set -euo pipefail
cd "$(dirname "$0")"
ROOT="$(cd ../.. && pwd)"
export LC_ALL=C

WORK="$(mktemp -d)"
pids=()
cleanup() {
  for p in "${pids[@]:-}"; do [[ -n "$p" ]] && kill "$p" 2>/dev/null || true; done
  wait 2>/dev/null || true
  rm -rf "$WORK"
}
trap cleanup EXIT INT TERM

fail=0
check() { # check DESCRIPTION CONDITION-COMMAND...
  local what="$1"; shift
  if "$@"; then echo "ok   $what"; else echo "FAIL $what" >&2; fail=1; fi
}
free_port() { python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1])'; }
wait_for() { # wait_for SECONDS COMMAND...
  local n=$(( $1 * 10 )); shift
  for _ in $(seq "$n"); do "$@" && return 0; sleep 0.1; done
  return 1
}
logged() { grep -q "$2" "$1" 2>/dev/null; } # logged FILE PATTERN

if [[ -z "$(ls -A "$ROOT/ui/dist" 2>/dev/null)" ]]; then
  mkdir -p "$ROOT/ui/dist"
  cp "$ROOT/ui/fallback/index.html" "$ROOT/ui/dist/index.html"
fi
(cd "$ROOT/ts/registration" && pnpm install --frozen-lockfile >/dev/null)
(cd "$ROOT/py/gwag-registration" && uv sync --locked --quiet)
(cd "$ROOT" && go build -o "$WORK/gwag" ./gw/cmd/gwag)
GWAG="$WORK/gwag"

HTTP=$(free_port); CP=$(free_port); NATS=$(free_port); ROUTE=$(free_port)
mkdir "$WORK/gw"
(cd "$WORK/gw" && exec "$GWAG" up --addr "127.0.0.1:$HTTP" --control-plane "127.0.0.1:$CP" \
  --nats-listen "127.0.0.1:$NATS" --nats-cluster "127.0.0.1:$ROUTE" \
  --schema-registry --shutdown-delay 0 >"$WORK/gateway.log" 2>&1) &
pids+=($!)
SR="http://127.0.0.1:$HTTP/schema-registry"
wait_for 60 curl -sf "$SR/subjects" -o /dev/null || { cat "$WORK/gateway.log" >&2; echo "gateway did not start" >&2; exit 1; }
echo "== gateway: http 127.0.0.1:$HTTP, control plane 127.0.0.1:$CP"

services() { (cd "$WORK/gw" && "$GWAG" ctl services list --gateway "127.0.0.1:$CP"); }
has_service() { services | grep -q "^$1 "; }
no_service() { ! has_service "$1"; }

echo "== 1. default subject"
SUBJECT=gateway.controlplane.v1.ControlPlane
wait_for 30 curl -sf "$SR/subjects/$SUBJECT/versions/latest" -o "$WORK/subject.json"
python3 - "$WORK/subject.json" "$ROOT/gw/proto/controlplane/v1/control.proto" "$WORK/control.proto" <<'EOF'
import json, sys
s = json.load(open(sys.argv[1]))
assert s["subject"] == "gateway.controlplane.v1.ControlPlane", s["subject"]
assert s["schemaType"] == "PROTOBUF", s["schemaType"]
assert s["schema"] == open(sys.argv[2]).read(), "served schema differs from gw/proto/controlplane/v1/control.proto"
open(sys.argv[3], "w").write(s["schema"])
print("subject %s version %d id %d" % (s["subject"], s["version"], s["id"]))
EOF
check "served control.proto equals the file in the repo" test -s "$WORK/control.proto"

mkdir -p "$WORK/fetched/gw/proto/controlplane/v1" && cp "$WORK/control.proto" "$WORK/fetched/gw/proto/controlplane/v1/control.proto"
"$GWAG" gen --lang ts --module-dir "$ROOT/ts/registration" --out "$WORK/gen-ts" -I "$WORK/fetched" \
  --opt target=ts --opt import_extension=ts --opt erasable_syntax=true \
  gw/proto/controlplane/v1/control.proto >/dev/null
check "TypeScript client generated from the fetched file matches ts/registration/src/gen" \
  diff -r "$WORK/gen-ts" "$ROOT/ts/registration/src/gen"
"$GWAG" gen --lang python --module-dir "$ROOT/py/gwag-registration" --out "$WORK/gen-py" -I "$WORK/fetched" \
  --opt client_generation=async --opt server_generation=async \
  gw/proto/controlplane/v1/control.proto >/dev/null
check "Python client generated from the fetched file matches py/gwag-registration/.../gen" \
  diff -r -x __pycache__ "$WORK/gen-py" "$ROOT/py/gwag-registration/src/gwag_registration/gen"

TSSVC=(node "$ROOT/ts/registration/test/fixture-service.ts" --gateway "127.0.0.1:$CP")
PYSVC=("$ROOT/py/gwag-registration/.venv/bin/python" "$ROOT/py/gwag-registration/tests/fixture_service.py" --gateway "127.0.0.1:$CP")
ORDERS="$ROOT/examples/events/proto/orders/v1/orders.proto"
OPTIONS="$ROOT/gw/proto/ps/v1/options.proto"


# A listener's own small proto: the binding needs a service of its own.
listener_proto() {
  printf 'syntax = "proto3";\npackage %s.v1;\nmessage Ping { string id = 1; }\nservice Notify { rpc Get(Ping) returns (Ping); }\n' "$1" >"$WORK/$1.proto"
}
listener_proto pynotify
listener_proto tsnotify

echo "== 2. Python listener waits for the channels' declarer"
"${PYSVC[@]}" --namespace pynotify --proto "$WORK/pynotify.proto" \
  --subscribe orders.v1.OrderEvents=py >"$WORK/py.log" 2>&1 &
PY=$!; pids+=($PY)
wait_for 20 curl -sf "$SR/subjects" -o /dev/null
sleep 3
check "pynotify has not started (no orders yet)" bash -c "! grep -q REGISTERED '$WORK/py.log'"
check "pynotify logs what it waits for" logged "$WORK/py.log" "waiting: .*channel orders.v1.OrderEvents (not declared yet)"

echo "== 3. TypeScript orders declares the channels"
"${TSSVC[@]}" --namespace orders --proto "$ORDERS" \
  --import "gw/proto/ps/v1/options.proto=$OPTIONS" >"$WORK/ts-orders.log" 2>&1 &
TSO=$!; pids+=($TSO)
check "orders registered" wait_for 30 logged "$WORK/ts-orders.log" REGISTERED
check "pynotify became ready" wait_for 30 logged "$WORK/py.log" REGISTERED

echo "== 4. TypeScript listener"
"${TSSVC[@]}" --namespace tsnotify --proto "$WORK/tsnotify.proto" \
  --subscribe orders.v1.OrderEvents=ts >"$WORK/ts.log" 2>&1 &
TS=$!; pids+=($TS)
check "tsnotify registered" wait_for 30 logged "$WORK/ts.log" REGISTERED

echo "== 5. gateway view"
# The list is built from the registry KV watch, which applies a
# registration shortly after the client's Register returns.
for ns in orders pynotify tsnotify; do
  check "gwag ctl services list shows $ns" wait_for 10 has_service "$ns"
done
ADMIN="$(sed -n 's/.*admin token = \([0-9a-f]*\).*/\1/p' "$WORK/gateway.log" | head -n1)"
curl -sf -H "Authorization: Bearer $ADMIN" "http://127.0.0.1:$HTTP/api/admin/services" >"$WORK/admin-services.json"
check "admin API lists each listener's Subscribes" python3 - "$WORK/admin-services.json" <<'EOF'
import json, sys
d = json.load(open(sys.argv[1]))
rows = d if isinstance(d, list) else d.get("services", d.get("items", []))
want = {"pynotify": "py", "tsnotify": "ts"}
got = {}
for r in rows:
    if r.get("namespace") in want:
        got[r["namespace"]] = [(s["channel"], s["group"]) for s in r.get("subscribes", [])]
ok = all(got.get(ns) == [("orders.v1.OrderEvents", g)] for ns, g in want.items())
print(got)
sys.exit(0 if ok else 1)
EOF

echo "== 6. SIGTERM deregisters"
kill -TERM "$PY" "$TS"
check "listeners exited" wait_for 10 bash -c "! kill -0 $PY 2>/dev/null && ! kill -0 $TS 2>/dev/null"
# The TTL is 6s; gone within 3s means a Deregister, not an eviction.
check "pynotify and tsnotify gone from the gateway" wait_for 3 bash -c "$(declare -f services has_service no_service); WORK='$WORK'; GWAG='$GWAG'; CP='$CP'; no_service pynotify && no_service tsnotify"
check "orders still registered" has_service orders

if [[ $fail == 0 ]]; then echo "PASS"; else echo "FAILED" >&2; echo "--- gateway.log" >&2; tail -20 "$WORK/gateway.log" >&2; for f in py ts ts-orders; do echo "--- $f.log" >&2; tail -10 "$WORK/$f.log" >&2; done; exit 1; fi
