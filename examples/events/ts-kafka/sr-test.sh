#!/usr/bin/env bash
# Start a single-node Kafka and a Confluent Schema Registry in docker and
# check the TypeScript Schema Registry serde (@gwag/events-schemaregistry)
# against them, stop them.
#
#   examples/events/ts-kafka/sr-test.sh
#
# What it checks (docs/events-kafka.md, "Schema Registry options"):
#   - the serde tests of ts/events-schemaregistry against a real registry;
#   - Go and TypeScript read each other's records, and register and look
#     up the same schema (same id, one version);
#   - Confluent's own code reads what TypeScript writes (the Java
#     kafka-protobuf-console-consumer in the registry image, the
#     confluent-kafka Python ProtobufDeserializer) and TypeScript reads
#     what Confluent's serializers write (Java console producer, Python
#     ProtobufSerializer), none with Gwag-* headers;
#   - import (gwag gen --lang ts --registry, --check-lock) and shadow
#     (typeNames, gwag ctl schema check) against a topic another team writes.
#
# Needs docker, go, node >= 24, pnpm and uv (the Python checks in
# ../go-kafka/confluent). KAFKA_PORT (default 19092) and SR_PORT
# (default 18081) are the host ports; KAFKA_IMAGE and SR_IMAGE override
# the pinned images.
set -euo pipefail
cd "$(dirname "$0")"
HERE="$(pwd)"
ROOT="$(cd ../../.. && pwd)"
source ../kafka-stack.sh

command -v uv >/dev/null || { echo "uv is needed for the Confluent Python checks" >&2; exit 1; }

pnpm install --frozen-lockfile >/dev/null
pnpm -s typecheck
(cd "$ROOT/ts/events-schemaregistry" && pnpm install --frozen-lockfile >/dev/null && pnpm -s typecheck)
(cd "$ROOT/examples/events/ts" && pnpm install --frozen-lockfile >/dev/null)

stack_up sr
SR="$STACK_NAME-sr"

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
gwag() { (cd "$ROOT" && go run ./gw/cmd/gwag "$@"); }
go_sr() { (cd ../go-kafka && go run ./srinterop "$@"); }
py() { (cd ../go-kafka/confluent && uv run --locked --quiet python confluent_check.py "$@"); }
ts_sr() { pnpm -s srinterop "$@" 2> >(grep -v -e TimeoutNegativeWarning -e 'Timeout duration was set' -e 'trace-warnings' >&2); }
versions() { curl -fsS "$SR_URL/subjects/$1-value/versions"; }
java_read() { # java_read TOPIC: one record through Confluent's Java deserializer
  docker exec "$SR" timeout 60 kafka-protobuf-console-consumer --bootstrap-server "$STACK_NAME:9094" \
    --topic "$1" --from-beginning --max-messages 1 --property schema.registry.url=http://localhost:8081 2>/dev/null | grep "^{"
}

echo "== serde tests against $SR_IMAGE"
(cd "$ROOT/ts/events-schemaregistry" && pnpm -s test)

echo "== Go produces (own), TypeScript reads; TypeScript looks the same schema up"
go_sr produce go-shipped go-events
out="$(ts_sr consume go-shipped go-events 2)"
echo "$out"
expect "TypeScript reads Go" "$out" \
  "OrderShipped o-1001 via ups (key events.orders.o-1001.shipped, schema id" "gwag headers true)" \
  "OrderCreated o-1001 by ann (type from Gwag-Type header)"
ts_sr produce go-shipped go-events lookup
out="$(go_sr consume go-shipped go-events 4)"
echo "$out"
ids="$(grep -o 'schema id [0-9]*' <<<"$out" | sort -u)"
echo "schema ids seen by Go: $ids"
if [[ "$(grep -c . <<<"$ids")" != 1 ]]; then
  echo "FAIL Go-registered and TypeScript-framed records use different schema ids" >&2
  fail=1
fi
expect "same version" "$(versions go-shipped)" "[1]"

echo "== TypeScript produces (own), Confluent, Go and TypeScript read"
ts_sr produce ts-shipped ts-events
java_out=""
for topic in ts-shipped ts-events; do
  out="$(java_read "$topic")"
  echo "java kafka-protobuf-console-consumer $topic: $out"
  java_out+="$out"$'\n'
done
expect "Confluent Java deserializer reads TypeScript" "$java_out" \
  '{"orderId":"o-1001","carrier":"ups"}' '{"orderId":"o-1001","customer":"ann"}'
out="$(py consume ts-shipped ts-events)"
echo "$out"
expect "Confluent Python deserializer reads TypeScript" "$out" \
  "confluent orders.v1.OrderShipped: order_id=o-1001 carrier=ups" \
  "confluent orders.v1.OrderCreated: order_id=o-1001 customer=ann"
out="$(go_sr consume ts-shipped ts-events 2)"
echo "$out"
expect "Go reads TypeScript" "$out" \
  "OrderShipped o-1001 via ups (key events.orders.o-1001.shipped, schema id" "gwag headers true)" \
  "OrderCreated o-1001 by ann (type from Gwag-Type header)"
go_sr produce ts-shipped ts-events >/dev/null
expect "Go registers the schema TypeScript registered" "$(versions ts-shipped)" "[1]"
expect "Go registers the schema TypeScript registered" "$(versions ts-events)" "[1]"

echo "== TypeScript reads what Confluent's serializers wrote (no Gwag-* headers)"
py produce confluent-shipped confluent-events
out="$(ts_sr consume confluent-shipped confluent-events 2)"
echo "$out"
expect "TypeScript reads Confluent Python" "$out" \
  "OrderShipped o-2001 via dhl (key o-2001, schema id" "gwag headers false)" \
  "OrderCreated o-2001 by cy (type from schema id"
docker exec -i "$SR" sh -c 'timeout 60 kafka-protobuf-console-producer --bootstrap-server "$0:9094" --topic jv-shipped \
  --property schema.registry.url=http://localhost:8081 --property parse.key=true --property key.separator=: \
  --property key.serializer=org.apache.kafka.common.serialization.StringSerializer \
  --property "value.schema=syntax = \"proto3\"; package orders.v1; message OrderShipped { string order_id = 1; string carrier = 2; }"' \
  "$STACK_NAME" <<<'o-4001:{"orderId":"o-4001","carrier":"fedex"}' >/dev/null
out="$(ts_sr consume jv-shipped jv-events 1)"
echo "$out"
expect "TypeScript reads Confluent Java (schema registered as .proto text)" "$out" \
  "OrderShipped o-4001 via fedex (key o-4001, schema id" "gwag headers false)"

echo "== import: corp-orders written by Confluent's serializer, typed code from a pinned copy"
py produce-corp corp-orders
corp/gen.sh >/dev/null
pnpm -s exec tsc --noEmit -p corp
out="$(node corp/main.ts import corp-orders 2> >(grep -v -e TimeoutNegativeWarning -e 'Timeout duration was set' -e 'trace-warnings' >&2))"
echo "$out"
expect "import" "$out" \
  "import corp.orders.v1.Order o-3001 total 4200 EUR status PLACED skus [sku-1] (key o-3001" \
  "import corp.orders.v1.Order o-3002 total 999 EUR status PLACED skus [sku-2 sku-3] (key o-3002"
out="$(gwag gen --check-lock --import-dir "$HERE/corp/imported")"
expect "check-lock" "$out" "ok   $HERE/corp/imported/gwag.lock.json matches $SR_URL (1 subjects)"

echo "== shadow: gwag's own copy, checked against the subject"
out="$(node corp/main.ts shadow corp-orders 2> >(grep -v -e TimeoutNegativeWarning -e 'Timeout duration was set' -e 'trace-warnings' >&2))"
echo "$out"
expect "shadow" "$out" "shadow shop.v1.CorpOrder o-3001 total 4200 (key o-3001)" "shadow shop.v1.CorpOrder o-3002 total 999 (key o-3002)"
out="$(gwag ctl schema check --registry "corp=$SR_URL" -I "$ROOT/examples/events/go-kafka/corp/shadow" shop/v1/order.proto)"
echo "$out"
expect "schema check" "$out" "ok   shop.v1.CorpOrder (consumer) vs corp corp-orders-value@latest" \
  "ok   shop.v1.CorpOrderOut (producer) vs corp corp-orders-value@latest"

if [[ $fail == 0 ]]; then echo PASS; else echo FAILED >&2; exit 1; fi
