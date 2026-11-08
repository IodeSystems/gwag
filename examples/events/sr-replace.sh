#!/usr/bin/env bash
# gwag in place of a Confluent Schema Registry (docs/schema-registry.md).
#
#   examples/events/sr-replace.sh
#
# 1. Kafka and a Confluent registry in docker. Records go to Kafka
#    framed with the Confluent registry's ids, written by the Go,
#    Python and TypeScript serdes (AutoRegister) and by Confluent's own
#    Python and Java serializers.
# 2. A gateway (gwag up --schema-registry) imports the registry into
#    context .corp with `gwag schema import --into .corp`. The
#    Confluent registry is stopped.
# 3. Pointed at gwag's .corp context (/schema-registry/contexts/.corp):
#    the Go, Python and TypeScript serdes and Confluent's Python and
#    Java deserializers read the old records (same ids), and the gwag
#    serdes write new ones in lookup and latest mode with the imported
#    ids.
# 4. A gwag-owned channel registers in the default context: its id is
#    the same number as the id of a corp record (forced), and each
#    record decodes as its own schema through its own URL. Writes
#    need a token.
#
# Needs docker, go, uv, node >= 24 and pnpm. KAFKA_PORT (19092),
# SR_PORT (18081) and GW_PORT (18090; the next three ports are used
# too) are host ports.
set -Eeuo pipefail
trap 'echo "line $LINENO failed: $BASH_COMMAND" >&2' ERR
cd "$(dirname "$0")"
EV="$(pwd)"
ROOT="$(cd ../.. && pwd)"
source ./kafka-stack.sh
GW_PORT="${GW_PORT:-18090}"
for port in "$GW_PORT" $((GW_PORT + 1)) $((GW_PORT + 2)) $((GW_PORT + 3)); do
  if (exec 3<>"/dev/tcp/127.0.0.1/$port") 2>/dev/null; then
    echo "port $port is in use; set GW_PORT" >&2
    exit 1
  fi
done
for tool in uv node pnpm; do
  command -v "$tool" >/dev/null || { echo "$tool is needed" >&2; exit 1; }
done

(cd "$EV/ts-kafka" && pnpm install --frozen-lockfile >/dev/null)
for d in events events-kafka events-schemaregistry; do (cd "$ROOT/ts/$d" && pnpm install --frozen-lockfile >/dev/null); done
(cd "$EV/ts" && pnpm install --frozen-lockfile >/dev/null)
REBUILD=1 "$ROOT/bin/gwag" --help >/dev/null 2>&1 || true

GWDIR="$(mktemp -d)"
GW_PID=""
finish() {
  [[ -n "$GW_PID" ]] && kill "$GW_PID" 2>/dev/null && wait "$GW_PID" 2>/dev/null
  rm -rf "$GWDIR"
  stack_down
}
stack_up sr
trap finish EXIT INT TERM
CONFLUENT="$SR_URL"
SRC="$STACK_NAME-sr"

fail=0
expect() { # expect WHAT OUTPUT WANT...
  local what="$1" got="$2"
  shift 2
  for want in "$@"; do
    if grep -qF -- "$want" <<<"$got"; then
      echo "ok   $what: $want"
    else
      echo "FAIL $what: missing '$want'" >&2
      fail=1
    fi
  done
}
go_sr() { (cd "$EV/go-kafka" && go run ./srinterop "$@"); }
py_sr() { (cd "$EV/py-kafka" && uv run --quiet python srinterop.py "$@"); }
ts_sr() { (cd "$EV/ts-kafka" && pnpm -s srinterop "$@" 2> >(grep -v -e TimeoutNegativeWarning -e 'Timeout duration was set' -e 'trace-warnings' >&2 || true)); }
cf() { (cd "$EV/go-kafka/confluent" && uv run --locked --quiet python confluent_check.py "$@"); }
java_read() { # java_read TOPIC REGISTRY_URL: one record through Confluent's Java deserializer, from the host network
  docker run --rm --network host --entrypoint timeout "$SR_IMAGE" 90 kafka-protobuf-console-consumer \
    --bootstrap-server "127.0.0.1:$KAFKA_PORT" --topic "$1" --from-beginning --max-messages 1 \
    --property schema.registry.url="$2" 2>/dev/null | grep "^{"
}
id_of() { curl -fsS "$1/subjects/$2-value/versions/latest" | python3 -c 'import json,sys;print(json.load(sys.stdin)["id"])'; }

echo "== write records with the Confluent registry ($SR_IMAGE)"
# Burn ids first, so imported ids cannot line up with a fresh counter.
for i in 1 2 3; do
  curl -fsS -X POST -H 'Content-Type: application/vnd.schemaregistry.v1+json' "$CONFLUENT/subjects/filler-$i-value/versions" \
    -d "{\"schemaType\":\"PROTOBUF\",\"schema\":\"syntax = \\\"proto3\\\"; package filler; message F$i {}\"}" >/dev/null
done
go_sr produce go-shipped go-events
py_sr produce py-shipped py-events
ts_sr produce ts-shipped ts-events
cf produce cf-shipped cf-events
cf produce-corp corp-orders
docker exec -i "$SRC" sh -c 'timeout 60 kafka-protobuf-console-producer --bootstrap-server "$0:9094" --topic jv-shipped \
  --property schema.registry.url=http://localhost:8081 --property parse.key=true --property key.separator=: \
  --property key.serializer=org.apache.kafka.common.serialization.StringSerializer \
  --property "value.schema=syntax = \"proto3\"; package orders.v1; message OrderShipped { string order_id = 1; string carrier = 2; }"' \
  "$STACK_NAME" <<<'o-4001:{"orderId":"o-4001","carrier":"fedex"}' >/dev/null
declare -A want_id
for s in go-shipped go-events py-shipped ts-shipped ts-events cf-shipped corp-orders jv-shipped; do
  want_id[$s]="$(id_of "$CONFLUENT" "$s")"
done
echo "Confluent ids: $(for s in "${!want_id[@]}"; do printf '%s=%s ' "$s" "${want_id[$s]}"; done)"

echo "== gwag up --schema-registry; gwag schema import"
"$ROOT/bin/gwag" up --addr "127.0.0.1:$GW_PORT" --control-plane "127.0.0.1:$((GW_PORT + 1))" \
  --nats-listen "127.0.0.1:$((GW_PORT + 2))" --nats-cluster "127.0.0.1:$((GW_PORT + 3))" \
  --data-dir "$GWDIR" --no-context-write --shutdown-delay 0 --schema-registry --schema-registry-writes >"$GWDIR/gwag.log" 2>&1 &
GW_PID=$!
GW="http://127.0.0.1:$GW_PORT/schema-registry"
GW_CORP="$GW/contexts/.corp"
for _ in $(seq 120); do
  curl -fsS "$GW/subjects" >/dev/null 2>&1 && break
  sleep 0.5
done
curl -fsS "$GW/subjects" >/dev/null || { cat "$GWDIR/gwag.log" >&2; exit 1; }
TOKEN="$(cat "$GWDIR/admin-token")"
GW_AUTH="http://x:$TOKEN@127.0.0.1:$GW_PORT/schema-registry"
out="$("$ROOT/bin/gwag" schema import --from "$CONFLUENT" --to "$GW" --bearer "$TOKEN" 2>&1 || true)"
expect "import needs a context" "$out" "--into CONTEXT"
out="$(curl -sS -u "x:$TOKEN" -X POST -H 'Content-Type: application/vnd.schemaregistry.v1+json' "$GW/gwag/import" -d '{"schemas":[],"versions":[]}')"
expect "import into the default context refused" "$out" '"error_code":42205'
out="$("$ROOT/bin/gwag" schema import --from "$CONFLUENT" --to "$GW" --into .corp --bearer "$TOKEN")"
echo "$out"
expect "import" "$out" "versions added" "/contexts/.corp"
out="$("$ROOT/bin/gwag" schema import --from "$CONFLUENT" --to "$GW" --into .corp --bearer "$TOKEN")"
echo "$out"
expect "import again" "$out" "0 schemas added, 0 versions added"
for s in "${!want_id[@]}"; do
  got="$(id_of "$GW_CORP" "$s")"
  if [[ "$got" == "${want_id[$s]}" ]]; then
    echo "ok   id preserved: $s-value id $got"
  else
    echo "FAIL $s-value: gwag id $got, Confluent id ${want_id[$s]}" >&2
    fail=1
  fi
done
docker stop "$SRC" >/dev/null
echo "== Confluent registry stopped; the corp topics use $GW_CORP"
export SR_URL="$GW_CORP"

out="$(curl -fsS "$GW/contexts")"
expect "contexts" "$out" '[".",".corp"]'
out="$(curl -sS -X POST -H 'Content-Type: application/vnd.schemaregistry.v1+json' "$GW/subjects/new-value/versions" \
  -d '{"schemaType":"PROTOBUF","schema":"syntax = \"proto3\"; message N {}"}')"
expect "register without a token refused" "$out" '"error_code":40101'

echo "== old records decode against gwag"
out="$(go_sr consume cf-shipped cf-events 2)"
echo "$out"
expect "Go reads Confluent Python records" "$out" "OrderShipped o-2001 via dhl (key o-2001, schema id ${want_id[cf-shipped]}," \
  "OrderCreated o-2001 by cy (type from schema id"
out="$(go_sr consume ts-shipped ts-events 2)"
echo "$out"
expect "Go reads TypeScript records" "$out" "OrderShipped o-1001 via ups" "OrderCreated o-1001 by ann"
out="$(py_sr consume go-shipped go-events 2)"
echo "$out"
expect "Python reads Go records" "$out" "OrderShipped o-1001 via ups (key events.orders.o-1001.shipped, schema id ${want_id[go-shipped]}," \
  "OrderCreated o-1001 by ann"
out="$(ts_sr consume py-shipped py-events 2)"
echo "$out"
expect "TypeScript reads Python records" "$out" "OrderShipped o-1001 via ups" "OrderCreated o-1001 by ann"
out="$(ts_sr consume jv-shipped jv-events 1)"
echo "$out"
expect "TypeScript reads the Java record (schema registered as .proto text)" "$out" \
  "OrderShipped o-4001 via fedex (key o-4001, schema id ${want_id[jv-shipped]},"
out="$(cf consume go-shipped go-events)"
echo "$out"
expect "Confluent Python deserializer reads via gwag" "$out" \
  "confluent orders.v1.OrderShipped: order_id=o-1001 carrier=ups" \
  "confluent orders.v1.OrderCreated: order_id=o-1001 customer=ann"
java_out=""
for topic in go-shipped corp-orders; do
  o="$(java_read "$topic" "$GW_CORP")"
  echo "java kafka-protobuf-console-consumer $topic (registry $GW_CORP): $o"
  java_out+="$o"$'\n'
done
expect "Confluent Java deserializer reads via gwag" "$java_out" \
  '{"orderId":"o-1001","carrier":"ups"}' '"orderId":"o-3001"'

echo "== gwag serdes write against gwag with the imported ids"
go_sr produce go-shipped go-events lookup
go_sr produce go-shipped go-events latest
py_sr produce py-shipped py-events lookup
py_sr produce py-shipped py-events latest
ts_sr produce ts-shipped ts-events lookup
ts_sr produce ts-shipped ts-events latest
# Each pair is read by a runtime whose consumer group has not read it
# yet (a group resumes from its committed offsets).
for pair in "go_sr go-shipped go-events" "go_sr py-shipped py-events" "py_sr ts-shipped ts-events"; do
  set -- $pair
  out="$("$1" consume "$2" "$3" 6)"
  ids="$(grep -o 'schema id [0-9]*' <<<"$out" | sort -u | tr '\n' ' ')"
  want="schema id ${want_id[$2]} "
  shift
  if [[ "$ids" == "$want" ]]; then
    echo "ok   $1: records from AutoRegister (Confluent), lookup and latest (gwag) all carry $want"
  else
    echo "FAIL $1: ids seen [$ids], want [$want]" >&2
    echo "$out" >&2
    fail=1
  fi
done

echo "== a gwag-owned channel in the default context, ids clashing with .corp's"
# The default context holds the control plane schema (id 1). Burn ids
# so the owned channel's orders.proto gets the id of the corp-orders
# records (corp.orders.v1.Order in .corp): options.proto takes T-1,
# orders.proto T.
T="${want_id[corp-orders]}"
cp_id="$(curl -fsS "$GW/subjects/gateway.controlplane.v1.ControlPlane/versions/latest" | python3 -c 'import json,sys;print(json.load(sys.stdin)["id"])')"
expect "control plane is the first default-context schema" "id $cp_id" "id 1"
for i in $(seq 2 $((T - 2))); do
  curl -fsS -u "x:$TOKEN" -X POST -H 'Content-Type: application/vnd.schemaregistry.v1+json' "$GW/subjects/gwag-filler-$i-value/versions" \
    -d "{\"schemaType\":\"PROTOBUF\",\"schema\":\"syntax = \\\"proto3\\\"; package gwagfiller; message G$i {}\"}" >/dev/null
done
SR_URL="$GW_AUTH" go_sr produce own-shipped own-events
own_id="$(id_of "$GW" own-shipped)"
if [[ "$own_id" == "$T" ]]; then
  echo "ok   own-shipped-value is default-context id $own_id, the id of corp-orders-value in .corp"
else
  echo "FAIL own-shipped-value id $own_id, want $T (to clash with corp-orders-value)" >&2
  fail=1
fi
expect "default id $T is gwag's schema" "$(curl -fsS "$GW/schemas/ids/$T")" "package orders.v1"
expect ".corp id $T is corp's schema" "$(curl -fsS "$GW_CORP/schemas/ids/$T")" "package corp.orders.v1"
out="$(SR_URL="$GW" go_sr consume own-shipped own-events 2)"
echo "$out"
expect "Go reads the owned channel via $GW" "$out" "OrderShipped o-1001 via ups (key events.orders.o-1001.shipped, schema id $T,"
out="$(SR_URL="$GW" py_sr consume own-shipped own-events 2)"
expect "Python reads the owned channel via $GW" "$out" "OrderShipped o-1001 via ups (key events.orders.o-1001.shipped, schema id $T,"
o="$(java_read own-shipped "$GW")"
echo "java own-shipped (registry $GW): $o"
expect "Confluent Java reads the owned channel via $GW" "$o" '{"orderId":"o-1001","carrier":"ups"}'
o="$(java_read corp-orders "$GW_CORP")"
echo "java corp-orders (registry $GW_CORP): $o"
expect "Confluent Java reads corp id $T via $GW_CORP" "$o" '"orderId":"o-3001"'
# Confluent's ?subject= rule: at the root URL, a subject only .corp
# holds names .corp's id.
o="$(java_read corp-orders "$GW")"
echo "java corp-orders (registry $GW): $o"
expect "Confluent Java reads corp id $T via $GW (subject in .corp only)" "$o" '"orderId":"o-3001"'

if [[ $fail == 0 ]]; then echo PASS; else echo FAILED >&2; exit 1; fi
