#!/usr/bin/env bash
# Start a single-node Kafka and a Confluent Schema Registry in docker,
# run the Python Kafka backend tests (py/gwag-events-kafka), the example
# and the Schema Registry interop against them, stop them.
#
#   examples/events/py-kafka/test.sh
#   SR=0 examples/events/py-kafka/test.sh   # Kafka only, no registry
#
# The Schema Registry part checks gwag's Python serde against Confluent
# and against Go (docs/events-kafka.md, "Schema Registry options"):
#   - Go produces with the registry serde, Python reads;
#   - Python produces, Confluent's Python ProtobufDeserializer and Go
#     read (the confluent-kafka project in ../go-kafka/confluent);
#   - Confluent's ProtobufSerializer produces (no Gwag-* headers),
#     Python reads with typed handlers.
# Then the three ways to use a topic: own (srinterop.py, gwag schema
# push), import (corp.py import, gwag gen --check-lock), shadow
# (corp.py shadow).
#
# Needs docker, uv and go (the Go side of the interop). KAFKA_PORT
# (default 19093) and SR_PORT (default 18083) are the host ports;
# KAFKA_IMAGE and SR_IMAGE override the pinned images (the same tags as
# go-kafka).
set -euo pipefail
cd "$(dirname "$0")"
HERE="$(pwd)"
ROOT="$(cd ../../.. && pwd)"
PY="$ROOT/py"

KAFKA_PORT="${KAFKA_PORT:-19093}"
KAFKA_IMAGE="${KAFKA_IMAGE:-apache/kafka:4.3.1}"
SR_PORT="${SR_PORT:-18083}"
SR_IMAGE="${SR_IMAGE:-confluentinc/cp-schema-registry:8.1.0}"
SR="${SR:-1}"
NAME="gwag-events-pykafka-$$"
NET="$NAME-net"

command -v uv >/dev/null || { echo "uv not found: https://docs.astral.sh/uv/" >&2; exit 1; }
for port in "$KAFKA_PORT" $([[ $SR == 1 ]] && echo "$SR_PORT"); do
  if (exec 3<>"/dev/tcp/127.0.0.1/$port") 2>/dev/null; then
    echo "port $port is in use; set KAFKA_PORT / SR_PORT" >&2
    exit 1
  fi
done
if [[ $SR == 1 ]] && ! command -v go >/dev/null; then
  echo "go is needed for the Schema Registry interop; install it or set SR=0" >&2
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
echo "== uv sync"
uv sync --locked --quiet
(cd "$PY/gwag-events-kafka" && uv sync --locked --quiet)
(cd "$PY/gwag-events-schemaregistry" && uv sync --locked --quiet)
echo "== py/gwag-events-kafka: mypy --strict, backend tests"
(cd "$PY/gwag-events-kafka" && uv run mypy src tests && uv run pytest -q)
echo "== example"
out="$(uv run python main.py)"
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
confluent() { (cd "$HERE/../go-kafka/confluent" && uv run --locked --quiet python confluent_check.py "$@"); }
gosr() { (cd "$HERE/../go-kafka" && go run ./srinterop "$@"); }
suffix="$$"

echo "== py/gwag-events-schemaregistry: mypy --strict, tests"
(cd "$PY/gwag-events-schemaregistry" && uv run mypy src tests && uv run pytest -q)

echo "== Go produces with the registry serde, Python reads"
gosr produce "go-shipped-$suffix" "go-events-$suffix"
out="$(uv run python srinterop.py consume "go-shipped-$suffix" "go-events-$suffix" 2)"
echo "$out"
expect "Python reads Go" "$out" \
  "OrderShipped o-1001 via ups (key events.orders.o-1001.shipped, schema id" "gwag headers True)" \
  "OrderCreated o-1001 by ann (type from Gwag-Type header)"

echo "== Python produces (AutoRegister), Confluent's Python deserializer and Go read"
uv run python srinterop.py produce "py-shipped-$suffix" "py-events-$suffix"
out="$(confluent consume "py-shipped-$suffix" "py-events-$suffix")"
echo "$out"
expect "Confluent Python deserializer reads gwag-py" "$out" \
  "confluent orders.v1.OrderShipped: order_id=o-1001 carrier=ups" \
  "confluent orders.v1.OrderCreated: order_id=o-1001 customer=ann"
out="$(gosr consume "py-shipped-$suffix" "py-events-$suffix" 2)"
echo "$out"
expect "Go reads Python" "$out" \
  "OrderShipped o-1001 via ups (key events.orders.o-1001.shipped, schema id" "gwag headers true)" \
  "OrderCreated o-1001 by ann (type from Gwag-Type header)"
# Same file, same bytes: the registry gives Go's and Python's schema one id.
go_id="$(curl -fsS "$SR_URL/subjects/go-shipped-$suffix-value/versions/1" | python3 -c 'import json,sys;print(json.load(sys.stdin)["id"])')"
py_id="$(curl -fsS "$SR_URL/subjects/py-shipped-$suffix-value/versions/1" | python3 -c 'import json,sys;print(json.load(sys.stdin)["id"])')"
if [[ "$go_id" == "$py_id" ]]; then
  echo "ok   Go and Python registered the same schema (id $go_id)"
else
  echo "FAIL Go registered id $go_id, Python id $py_id" >&2
  fail=1
fi

echo "== Confluent's Python serializer produces (no Gwag-* headers), Python reads typed"
confluent produce "cf-shipped-$suffix" "cf-events-$suffix"
out="$(uv run python srinterop.py consume "cf-shipped-$suffix" "cf-events-$suffix" 2)"
echo "$out"
expect "Python reads Confluent" "$out" \
  "OrderShipped o-2001 via dhl (key o-2001, schema id" "gwag headers False)" \
  "OrderCreated o-2001 by cy (type from schema id"

echo "== own: gwag schema push finds what the Python serde registered"
out="$(gwag schema push --registry "$SR_URL" --topic "orders.v1.OrderShipped=py-shipped-$suffix" \
  --topic "orders.v1.OrderEvents=py-events-$suffix" -I examples/events/proto orders/v1/orders.proto)"
echo "$out"
expect "schema push" "$out" "ok   orders.v1.OrderShipped subject py-shipped-$suffix-value version 1" \
  "ok   orders.v1.OrderEvents subject py-events-$suffix-value version 1"

echo "== import: corp-orders written by Confluent's serializer, typed code from a pinned copy"
confluent produce-corp "corp-orders"
./corp-gen.sh >/dev/null
echo "== mypy --strict (example, generated code)"
uv run mypy
out="$(uv run python corp.py import "corp-orders")"
echo "$out"
expect "import" "$out" \
  "import corp.orders.v1.Order o-3001 total 4200 EUR status PLACED skus ['sku-1'] (key o-3001" \
  "import corp.orders.v1.Order o-3002 total 999 EUR status PLACED skus ['sku-2', 'sku-3'] (key o-3002"
out="$(gwag gen --check-lock --import-dir "$HERE/imported")"
expect "check-lock" "$out" "ok   $HERE/imported/gwag.lock.json matches $SR_URL (1 subjects)"

echo "== shadow: gwag's own copy, checked against the subject"
out="$(uv run python corp.py shadow "corp-orders")"
echo "$out"
expect "shadow" "$out" "shadow shop.v1.CorpOrder o-3001 total 4200 (key o-3001)" "shadow shop.v1.CorpOrder o-3002 total 999 (key o-3002)"

if [[ $fail == 0 ]]; then echo PASS; else echo FAILED >&2; exit 1; fi
