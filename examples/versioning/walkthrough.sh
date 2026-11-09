#!/usr/bin/env bash
# The life of orders v1, end to end, against a single `bin/gwag up`:
#
#   1. orders serves v1 and v2 from one process and registers both.
#   2. The SDL marks v1 @deprecated; an operator adds the sunset date.
#   3. Old clients call v1, new clients call v2 (Go and TypeScript).
#   4. Usage per version and per caller (X-Caller-Id).
#   5. billing (TypeScript) depends on orders v1; Acme (outside the
#      gateway's view) is declared as an external dependent on v1.
#   6. Retiring v1 is refused: the drain of the v1 line is blocked,
#      the dependency graph says why.
#   7. billing moves to v2, Acme's pin moves to v2.
#   8. The drain completes; once v1 has had no traffic for a minute the
#      graph marks it sunset-able.
#   9. orders restarts without v1; v1 is gone from the schema.
#  10. The code change that follows.
#
# Needs go, node >= 24, pnpm, curl. Takes about two minutes (step 8
# waits out the graph's one-minute usage window). Ports: PORT_BASE
# (default 18620) to PORT_BASE+3 for HTTP, PORT_BASE+30000 for the
# control plane.
set -euo pipefail
cd "$(dirname "$0")"
HERE="$(pwd)"
ROOT="$(cd ../.. && pwd)"

BASE="${PORT_BASE:-18620}"
HTTP=$BASE
ORDERS=$((BASE + 1))
NATS=$((BASE + 2))
ROUTE=$((BASE + 3))
CP=$((BASE + 30000))
GW="http://localhost:$HTTP"
RUN="$HERE/.run/walk"

pids=()
cleanup() {
  for p in "${pids[@]}"; do kill "$p" 2>/dev/null || true; done
  wait 2>/dev/null || true
}
trap cleanup EXIT

step() { printf '\n== %s\n' "$*"; }
fail() { echo "FAIL $*" >&2; exit 1; }
# want SUBSTRING TEXT: TEXT must contain SUBSTRING.
want() { grep -qF -- "$1" <<<"$2" || fail "missing '$1' in: $2"; echo "ok   $1"; }
wait_for() { # wait_for SECONDS DESCRIPTION CMD...
  local n=$(( $1 * 4 )) what=$2; shift 2
  for ((i = 0; i < n; i++)); do "$@" >/dev/null 2>&1 && return 0; sleep 0.25; done
  fail "timed out waiting for $what"
}
json() { node -e "const d=JSON.parse(require('fs').readFileSync(0,'utf8')); $1"; }

rm -rf "$RUN"
mkdir -p "$RUN"

step "build"
# bin/gwag runs a cached build that does not notice source changes;
# rebuild it from this checkout (REBUILD=0 keeps the cached one).
if [[ "${REBUILD:-1}" != 0 ]]; then
  REBUILD=1 "$ROOT/bin/gwag" up -h >/dev/null 2>&1 || true
fi
unset REBUILD
go build -o "$RUN/orders" ./cmd/orders
go build -o "$RUN/old-client" ./cmd/old-client
go build -o "$RUN/new-client" ./cmd/new-client
(cd "$ROOT/ts/registration" && pnpm install --frozen-lockfile >/dev/null)
(cd ts && pnpm install --frozen-lockfile >/dev/null)

step "1. gateway and orders (v1 + v2, one process)"
"$ROOT/bin/gwag" up --addr ":$HTTP" --control-plane ":$CP" \
  --nats-listen ":$NATS" --nats-cluster ":$ROUTE" --data-dir "$RUN/gw" \
  --no-context-write --shutdown-delay 0 --caller-id-public >"$RUN/gw.log" 2>&1 &
pids+=($!)
wait_for 60 "gateway health" curl -fsS "$GW/api/health"
TOKEN="$(cat "$RUN/gw/admin-token")"
admin() { curl -fsS -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' "$@"; }

"$RUN/orders" -listen ":$ORDERS" -gateway "localhost:$CP" >"$RUN/orders.log" 2>&1 &
orders_pid=$!
pids+=($orders_pid)
wait_for 30 "orders registration" grep -q "registered orders/v2" "$RUN/orders.log"
cat "$RUN/orders.log"

step "2. v1 is @deprecated in the SDL; add the sunset date"
sdl="$(curl -fsS "$GW/api/schema/graphql")"
want 'v1: OrdersV1QueryNamespace! @deprecated(reason: "v2 is current")' "$sdl"
admin -X POST "$GW/api/admin/services/orders/v1/deprecate" \
  -d '{"reason":"orders v1 ends 2027-06-30; move to v2 (docs/versioning.md)"}' >/dev/null
sdl="$(curl -fsS "$GW/api/schema/graphql")"
want 'v1: OrdersV1QueryNamespace! @deprecated(reason: "orders v1 ends 2027-06-30; move to v2 (docs/versioning.md)")' "$sdl"

step "3. old clients on v1, new clients on v2"
out="$("$RUN/old-client" -grpc "localhost:$CP")"; echo "$out"
want "v1 getOrder o-1: amount=12.50 customerName=\"Ada Lovelace\"" "$out"
want "v1 listOrders: 2 orders (USD only)" "$out"
out="$(cd ts && GATEWAY="$GW" node src/old-client.ts)"; echo "$out"
want "v1 getOrder o-2: amount=9.99 status=closed" "$out"
want "v1 getOrder o-3: HTTP 409" "$out"
want "order o-3 is in EUR; v1 shows only USD orders, use v2" "$out"
out="$("$RUN/new-client" -grpc "localhost:$CP")"; echo "$out"
want "v2 getOrder o-3: amountCents=4200 currency=EUR customer=\"Edsger Dijkstra\"" "$out"
want "v2 refundOrder o-3: refundedCents=200" "$out"
out="$(cd ts && GATEWAY="$GW" node src/new-client.ts)"; echo "$out"
want "v2 listOrders EUR: o-3 amountCents=4200 currency=EUR customer=Edsger Dijkstra" "$out"

step "4. who still calls v1 (deprecatedStats) and who calls v2 (usage)"
out="$(admin "$GW/api/admin/services/deprecated/stats?window=1h" | json '
  for (const s of d.services) for (const m of s.methods) for (const c of m.callers)
    console.log(`${s.namespace}/${s.version} ${m.method} ${c.caller} ${c.count}`);')"
echo "$out"
want "orders/v1 GET /v1/orders/{id} old-go-client 1" "$out"
want "orders/v1 GET /v1/orders/{id} old-ts-client 2" "$out"
want "orders/v1 GET /v1/orders old-go-client 1" "$out"
out="$(admin "$GW/api/admin/services/usage?window=1h&namespace=orders&version=v2" | json '
  for (const e of d.edges) console.log(`${e.namespace}/${e.version} ${e.method} ${e.caller} ${e.count}`);')"
echo "$out"
want "orders/v2 GET /v2/orders/{id} new-go-client 1" "$out"
want "orders/v2 GET /v2/orders new-ts-client 1" "$out"

step "5. dependents of v1: billing (registered) and Acme (declared)"
(cd ts && GATEWAY="$GW" CONTROL_PLANE="localhost:$CP" ORDERS_LINE=v1 node src/dependent.ts) >"$RUN/billing.log" 2>&1 &
billing_pid=$!
pids+=($billing_pid)
wait_for 30 "billing registration" grep -q "billing due o-1" "$RUN/billing.log"
cat "$RUN/billing.log"
review="$(node -e 'console.log(new Date(Date.now()+180*864e5).toISOString().slice(0,10)+"T00:00:00Z")')"
admin -X PUT "$GW/api/admin/external-dependents/acme" -d "{\"name\":\"Acme mobile app\",\"kind\":\"app\",
  \"owner\":{\"team\":\"mobile\",\"contact\":\"#mobile\"},
  \"depends\":[{\"service\":\"orders\",\"lines\":[\"v1\"]}],
  \"note\":\"app 4.2 calls v1; 5.0 moves to v2\",\"reviewAt\":\"$review\"}" >/dev/null
echo "declared external dependent acme on orders v1 (review $review)"

step "6. retiring v1 is refused"
graph() { admin "$GW/api/admin/graph?window=1m" | json '
  const n = d.nodes.find(n => n.namespace === "orders" && n.version === "v1");
  console.log(`orders/v1 sunsetable=${n.sunsetable}: ${n.sunsetReason}`);'; }
out="$(graph)"; echo "$out"
want "orders/v1 sunsetable=false" "$out"
want "billing/v1 accepts orders [v1]: no other ready line" "$out"
want "Acme mobile app" "$out"
out="$("$ROOT/bin/gwag" drain "orders-v1@http://localhost:$ORDERS" --gateway "localhost:$CP")"; echo "$out"
want "blocked" "$out"
"$ROOT/bin/gwag" drain "orders-v1@http://localhost:$ORDERS" --cancel --gateway "localhost:$CP"

step "7. billing moves to v2; Acme's pin moves to v2"
kill -TERM "$billing_pid"
wait "$billing_pid" 2>/dev/null || true
(cd ts && GATEWAY="$GW" CONTROL_PLANE="localhost:$CP" ORDERS_LINE=v2 node src/dependent.ts) >"$RUN/billing.log" 2>&1 &
pids+=($!)
wait_for 30 "billing on v2" grep -q "billing due o-1" "$RUN/billing.log"
cat "$RUN/billing.log"
admin -X PUT "$GW/api/admin/external-dependents/acme" -d "{\"name\":\"Acme mobile app\",\"kind\":\"app\",
  \"owner\":{\"team\":\"mobile\",\"contact\":\"#mobile\"},
  \"depends\":[{\"service\":\"orders\",\"lines\":[\"v2\"]}],
  \"note\":\"app 5.0 on v2 since $(date -u +%F)\",\"reviewAt\":\"$review\"}" >/dev/null
echo "acme now pins orders v2"

step "8. drain of v1 completes; v1 becomes sunset-able after a quiet minute"
"$ROOT/bin/gwag" drain "orders-v1@http://localhost:$ORDERS" --wait --timeout 30s --gateway "localhost:$CP" | tee "$RUN/drain.out"
want "safe_to_stop" "$(cat "$RUN/drain.out")"
out="$(graph)"; echo "$out"
echo "waiting for the 1m usage window to pass..."
sunsetable() { graph | grep -q "sunsetable=true"; }
wait_for 90 "orders v1 sunset-able" sunsetable
out="$(graph)"; echo "$out"
want "orders/v1 sunsetable=true: every dependent has another ready line" "$out"

step "9. orders restarts without v1"
kill -INT "$orders_pid"
wait "$orders_pid" 2>/dev/null || true
"$RUN/orders" -listen ":$ORDERS" -gateway "localhost:$CP" -no-v1 >"$RUN/orders.log" 2>&1 &
pids+=($!)
wait_for 30 "orders v2-only registration" grep -q "registered orders/v2" "$RUN/orders.log"
cat "$RUN/orders.log"
no_v1() { ! curl -fsS "$GW/api/schema/graphql" | grep -q OrdersV1QueryNamespace; }
wait_for 10 "v1 to leave the schema" no_v1
echo "ok   v1 is gone from the SDL"
if out="$("$RUN/old-client" -grpc "localhost:$CP" 2>&1)"; then fail "old client still works: $out"; fi
echo "old client now: $out"
code="$(curl -s -o /dev/null -w '%{http_code}' "$GW/api/ingress/v1/orders/o-1")"
echo "REST GET /api/ingress/v1/orders/o-1: HTTP $code"
[[ "$code" == 404 ]] || fail "v1 REST after retire: HTTP $code, want 404"
out="$("$RUN/new-client" -grpc "localhost:$CP")"
want "v2 getOrder o-3" "$out"
admin -X DELETE "$GW/api/admin/external-dependents/acme" >/dev/null || true

step "10. the code change"
cat <<'EOF'
v1 is no longer registered anywhere. Delete its code and its frozen files:

  git rm -r internal/api/v1 api/orders/v1 testdata/contract/v1 \
            cmd/legacy internal/cases clients/go/ordersv1 cmd/old-client \
            ts/src/gen/orders-v1.ts ts/src/old-client.ts
  # internal/server/server.go: drop the v1 import, Options.NoV1 and the
  #   v1 block in New; cmd/orders: drop -no-v1
  # ts/src/dependent.ts: drop the v1 path
  # snapshot.sh: loop over v2 only
  ./snapshot.sh update --allow-breaking   # api/orders/schema.graphql loses v1

Files that import v1 code today:
EOF
(cd "$HERE" && grep -rl --exclude-dir=node_modules --exclude-dir=.run --exclude=walkthrough.sh -e 'internal/api/v1"' -e 'clients/go/ordersv1"' -e 'gen/orders-v1.ts' . | sort)

echo
echo "walkthrough passed"
