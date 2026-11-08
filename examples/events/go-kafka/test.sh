#!/usr/bin/env bash
# Start a single-node Kafka and a Confluent Schema Registry in docker,
# run the Kafka backend tests (events/kafka) and the examples against
# them, stop them.
#
#   examples/events/go-kafka/test.sh
#   SR=0 examples/events/go-kafka/test.sh   # Kafka only, no registry
#
# The Schema Registry part checks gwag's framing against Confluent's
# own code: the Java kafka-protobuf-console-consumer in the registry
# image and the confluent-kafka Python client (confluent/, run with uv)
# read what gwag writes, and gwag reads what the Python serializer
# writes. Then the three ways to use a topic (docs/events-kafka.md, "Who
# produces the topic?"): own (srinterop, gwag schema push), import
# (corp import, gwag gen --check-lock), shadow (corp shadow, gwag
# schema check).
#
# Needs docker and go; uv for the registry part. KAFKA_PORT (default
# 19092) and SR_PORT (default 18081) are the host ports; KAFKA_IMAGE
# and SR_IMAGE override the pinned images.
set -euo pipefail
cd "$(dirname "$0")"
HERE="$(pwd)"
ROOT="$(cd ../../.. && pwd)"

KAFKA_PORT="${KAFKA_PORT:-19092}"
KAFKA_IMAGE="${KAFKA_IMAGE:-apache/kafka:4.3.1}"
SR_PORT="${SR_PORT:-18081}"
SR_IMAGE="${SR_IMAGE:-confluentinc/cp-schema-registry:8.1.0}"
SR="${SR:-1}"
NAME="gwag-events-kafka-$$"
NET="$NAME-net"

for port in "$KAFKA_PORT" $([[ $SR == 1 ]] && echo "$SR_PORT"); do
  if (exec 3<>"/dev/tcp/127.0.0.1/$port") 2>/dev/null; then
    echo "port $port is in use; set KAFKA_PORT / SR_PORT" >&2
    exit 1
  fi
done
if [[ $SR == 1 ]] && ! command -v uv >/dev/null; then
  echo "uv is needed for the Schema Registry checks; install it or set SR=0" >&2
  exit 1
fi

cleanup() {
  docker rm -f "$NAME" "$NAME-sr" >/dev/null 2>&1 || true
  docker network rm "$NET" >/dev/null 2>&1 || true
}
trap cleanup EXIT INT TERM

docker network create "$NET" >/dev/null
# PLAINTEXT is for the host (tests, examples); INTERNAL for the
# registry container.
docker run -d --name "$NAME" --network "$NET" -p "127.0.0.1:$KAFKA_PORT:9092" \
  -e KAFKA_NODE_ID=1 \
  -e KAFKA_PROCESS_ROLES=broker,controller \
  -e KAFKA_LISTENERS=PLAINTEXT://:9092,CONTROLLER://:9093,INTERNAL://:9094 \
  -e KAFKA_ADVERTISED_LISTENERS="PLAINTEXT://127.0.0.1:$KAFKA_PORT,INTERNAL://$NAME:9094" \
  -e KAFKA_CONTROLLER_LISTENER_NAMES=CONTROLLER \
  -e KAFKA_INTER_BROKER_LISTENER_NAME=INTERNAL \
  -e KAFKA_LISTENER_SECURITY_PROTOCOL_MAP=CONTROLLER:PLAINTEXT,PLAINTEXT:PLAINTEXT,INTERNAL:PLAINTEXT \
  -e KAFKA_CONTROLLER_QUORUM_VOTERS=1@localhost:9093 \
  -e KAFKA_OFFSETS_TOPIC_REPLICATION_FACTOR=1 \
  -e KAFKA_TRANSACTION_STATE_LOG_REPLICATION_FACTOR=1 \
  -e KAFKA_TRANSACTION_STATE_LOG_MIN_ISR=1 \
  -e KAFKA_GROUP_INITIAL_REBALANCE_DELAY_MS=0 \
  "$KAFKA_IMAGE" >/dev/null
if [[ $SR == 1 ]]; then
  docker run -d --name "$NAME-sr" --network "$NET" -p "127.0.0.1:$SR_PORT:8081" \
    -e SCHEMA_REGISTRY_HOST_NAME=sr \
    -e SCHEMA_REGISTRY_LISTENERS=http://0.0.0.0:8081 \
    -e SCHEMA_REGISTRY_KAFKASTORE_BOOTSTRAP_SERVERS="PLAINTEXT://$NAME:9094" \
    "$SR_IMAGE" >/dev/null
fi

# The advertised listener is the host port, which does not exist inside
# the container, so wait on the log line instead of a client inside it.
echo "== wait for kafka on :$KAFKA_PORT"
for _ in $(seq 120); do
  docker logs "$NAME" 2>&1 | grep -q "Kafka Server started" && break
  sleep 0.5
done
docker logs "$NAME" 2>&1 | grep -q "Kafka Server started" || { docker logs "$NAME" >&2; exit 1; }

export KAFKA_BROKERS="127.0.0.1:$KAFKA_PORT"
echo "== backend tests"
(cd "$ROOT/events/kafka" && go test -count=1 ./...)
echo "== example"
out="$(go run .)"
echo "$out"
for want in "OrderEvents   o-1001 created by ann" "OrderEvents   o-1002 cancelled: out of stock" \
  "Payments      eu/card captured 4200 for o-1001" "OrderShipped  o-1001 via ups"; do
  grep -qF -- "$want" <<<"$out" || { echo "FAIL missing '$want'" >&2; exit 1; }
done

if [[ $SR != 1 ]]; then
  echo "SKIP Schema Registry checks (SR=0)"
  echo PASS
  exit 0
fi

export SR_URL="http://127.0.0.1:$SR_PORT"
echo "== wait for schema registry on :$SR_PORT"
for _ in $(seq 240); do
  curl -fsS "$SR_URL/subjects" >/dev/null 2>&1 && break
  sleep 0.5
done
curl -fsS "$SR_URL/subjects" >/dev/null || { docker logs "$NAME-sr" >&2; exit 1; }

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
py() { (cd "$HERE/confluent" && uv run --locked --quiet python confluent_check.py "$@"); }

echo "== schemaregistry tests against $SR_IMAGE"
(cd "$ROOT" && go test -count=1 ./events/schemaregistry/...)

echo "== own: gwag writes with the registry serde (AutoRegister)"
java_out=""
go run ./srinterop produce gwag-shipped gwag-events
for topic in gwag-shipped gwag-events; do
  out="$(docker exec "$NAME-sr" timeout 60 kafka-protobuf-console-consumer --bootstrap-server "$NAME:9094" \
    --topic "$topic" --from-beginning --max-messages 1 --property schema.registry.url=http://localhost:8081 2>/dev/null | grep "^{")"
  echo "java kafka-protobuf-console-consumer $topic: $out"
  java_out+="$out"$'\n'
done
expect "Confluent Java deserializer reads gwag" "$java_out" \
  '{"orderId":"o-1001","carrier":"ups"}' '{"orderId":"o-1001","customer":"ann"}'
out="$(py consume gwag-shipped gwag-events)"
echo "$out"
expect "Confluent Python deserializer reads gwag" "$out" \
  "confluent orders.v1.OrderShipped: order_id=o-1001 carrier=ups" \
  "confluent orders.v1.OrderCreated: order_id=o-1001 customer=ann"

echo "== gwag reads what Confluent's Python serializer wrote (no Gwag-* headers)"
py produce confluent-shipped confluent-events
out="$(go run ./srinterop consume confluent-shipped confluent-events 2)"
echo "$out"
expect "gwag reads Confluent" "$out" \
  "OrderShipped o-2001 via dhl (key o-2001, schema id" "gwag headers false)" \
  "OrderCreated o-2001 by cy (type from schema id"

echo "== own: gwag schema push finds what the serde registered"
out="$(gwag schema push --registry "$SR_URL" --topic orders.v1.OrderShipped=gwag-shipped \
  --topic orders.v1.OrderEvents=gwag-events -I examples/events/proto orders/v1/orders.proto)"
echo "$out"
expect "schema push" "$out" "ok   orders.v1.OrderShipped subject gwag-shipped-value version 1" \
  "ok   orders.v1.OrderEvents subject gwag-events-value version 1"

echo "== import: corp-orders written by Confluent's serializer, typed code from a pinned copy"
py produce-corp corp-orders
corp/gen.sh
out="$(cd corp && go vet . && go run . import corp-orders)"
echo "$out"
expect "import" "$out" \
  "import corp.orders.v1.Order o-3001 total 4200 EUR status PLACED skus [sku-1] (key o-3001" \
  "import corp.orders.v1.Order o-3002 total 999 EUR status PLACED skus [sku-2 sku-3] (key o-3002"
out="$(gwag gen --check-lock --import-dir "$HERE/corp/imported")"
expect "check-lock" "$out" "ok   $HERE/corp/imported/gwag.lock.json matches $SR_URL (1 subjects)"

echo "== shadow: gwag's own copy, checked against the subject"
out="$(cd corp && go run . shadow corp-orders)"
echo "$out"
expect "shadow" "$out" "shadow shop.v1.CorpOrder o-3001 total 4200 (key o-3001)" "shadow shop.v1.CorpOrder o-3002 total 999 (key o-3002)"
out="$(gwag schema check --registry "corp=$SR_URL" -I "$HERE/corp/shadow" shop/v1/order.proto)"
echo "$out"
expect "schema check" "$out" "ok   shop.v1.CorpOrder (consumer) vs corp corp-orders-value@latest" \
  "ok   shop.v1.CorpOrderOut (producer) vs corp corp-orders-value@latest"
if out="$(gwag schema check --registry "corp=$SR_URL" -I "$HERE/corp/shadow" drift/v1/drift.proto)"; then
  echo "FAIL schema check passed on drift/v1/drift.proto" >&2
  fail=1
fi
echo "$out"
expect "schema check drift" "$out" \
  "total: field 2 is string, registry has message corp.common.v1.Money" \
  "coupon: field 9 is not in registry message corp.orders.v1.Order" \
  'FIELD_SCALAR_KIND_CHANGED'

if [[ $fail == 0 ]]; then echo PASS; else echo FAILED >&2; exit 1; fi
