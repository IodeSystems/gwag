#!/usr/bin/env bash
# Start a single-node Kafka in docker, run the Kafka backend tests and
# the example against it, stop it.
#
#   examples/events/go-kafka/test.sh
#
# Needs docker and go. KAFKA_PORT (default 19092) is the host port;
# KAFKA_IMAGE overrides the pinned image.
set -euo pipefail
cd "$(dirname "$0")"

KAFKA_PORT="${KAFKA_PORT:-19092}"
KAFKA_IMAGE="${KAFKA_IMAGE:-apache/kafka:4.3.1}"
NAME="gwag-events-kafka-$$"

if (exec 3<>"/dev/tcp/127.0.0.1/$KAFKA_PORT") 2>/dev/null; then
  echo "port $KAFKA_PORT is in use; set KAFKA_PORT" >&2
  exit 1
fi

cleanup() { docker rm -f "$NAME" >/dev/null 2>&1 || true; }
trap cleanup EXIT INT TERM

docker run -d --name "$NAME" -p "127.0.0.1:$KAFKA_PORT:9092" \
  -e KAFKA_NODE_ID=1 \
  -e KAFKA_PROCESS_ROLES=broker,controller \
  -e KAFKA_LISTENERS=PLAINTEXT://:9092,CONTROLLER://:9093 \
  -e KAFKA_ADVERTISED_LISTENERS="PLAINTEXT://127.0.0.1:$KAFKA_PORT" \
  -e KAFKA_CONTROLLER_LISTENER_NAMES=CONTROLLER \
  -e KAFKA_LISTENER_SECURITY_PROTOCOL_MAP=CONTROLLER:PLAINTEXT,PLAINTEXT:PLAINTEXT \
  -e KAFKA_CONTROLLER_QUORUM_VOTERS=1@localhost:9093 \
  -e KAFKA_OFFSETS_TOPIC_REPLICATION_FACTOR=1 \
  -e KAFKA_TRANSACTION_STATE_LOG_REPLICATION_FACTOR=1 \
  -e KAFKA_TRANSACTION_STATE_LOG_MIN_ISR=1 \
  -e KAFKA_GROUP_INITIAL_REBALANCE_DELAY_MS=0 \
  "$KAFKA_IMAGE" >/dev/null

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
go test -count=1 ./...
echo "== example"
out="$(go run .)"
echo "$out"
for want in "OrderEvents   o-1001 created by ann" "OrderEvents   o-1002 cancelled: out of stock" \
  "Payments      eu/card captured 4200 for o-1001" "OrderShipped  o-1001 via ups"; do
  grep -qF -- "$want" <<<"$out" || { echo "FAIL missing '$want'" >&2; exit 1; }
done
echo PASS
