# Sourced by the TypeScript Kafka scripts: starts a single-node Kafka
# (and, with stack_up sr, a Confluent Schema Registry) in docker, exports
# KAFKA_BROKERS (and SR_URL), and stops them on exit.
#
#   source "$(dirname "$0")/../kafka-stack.sh"
#   stack_up        # Kafka only
#   stack_up sr     # Kafka and Schema Registry
#
# KAFKA_PORT (default 19092) and SR_PORT (default 18081) are the host
# ports; KAFKA_IMAGE and SR_IMAGE override the pinned images. The
# containers are named gwag-events-ts-<pid>; STACK_NAME overrides.

KAFKA_PORT="${KAFKA_PORT:-19092}"
KAFKA_IMAGE="${KAFKA_IMAGE:-apache/kafka:4.3.1}"
SR_PORT="${SR_PORT:-18081}"
SR_IMAGE="${SR_IMAGE:-confluentinc/cp-schema-registry:8.1.0}"
STACK_NAME="${STACK_NAME:-gwag-events-ts-$$}"
STACK_NET="$STACK_NAME-net"

stack_down() {
  docker rm -f "$STACK_NAME" "$STACK_NAME-sr" >/dev/null 2>&1 || true
  docker network rm "$STACK_NET" >/dev/null 2>&1 || true
}

stack_up() {
  local sr="${1:-}" port
  for port in "$KAFKA_PORT" $([[ $sr == sr ]] && echo "$SR_PORT"); do
    if (exec 3<>"/dev/tcp/127.0.0.1/$port") 2>/dev/null; then
      echo "port $port is in use; set KAFKA_PORT / SR_PORT" >&2
      return 1
    fi
  done
  trap stack_down EXIT INT TERM
  docker network create "$STACK_NET" >/dev/null
  # PLAINTEXT is for the host (tests, examples); INTERNAL for the
  # registry container.
  docker run -d --name "$STACK_NAME" --network "$STACK_NET" -p "127.0.0.1:$KAFKA_PORT:9092" \
    -e KAFKA_NODE_ID=1 \
    -e KAFKA_PROCESS_ROLES=broker,controller \
    -e KAFKA_LISTENERS=PLAINTEXT://:9092,CONTROLLER://:9093,INTERNAL://:9094 \
    -e KAFKA_ADVERTISED_LISTENERS="PLAINTEXT://127.0.0.1:$KAFKA_PORT,INTERNAL://$STACK_NAME:9094" \
    -e KAFKA_CONTROLLER_LISTENER_NAMES=CONTROLLER \
    -e KAFKA_INTER_BROKER_LISTENER_NAME=INTERNAL \
    -e KAFKA_LISTENER_SECURITY_PROTOCOL_MAP=CONTROLLER:PLAINTEXT,PLAINTEXT:PLAINTEXT,INTERNAL:PLAINTEXT \
    -e KAFKA_CONTROLLER_QUORUM_VOTERS=1@localhost:9093 \
    -e KAFKA_OFFSETS_TOPIC_REPLICATION_FACTOR=1 \
    -e KAFKA_TRANSACTION_STATE_LOG_REPLICATION_FACTOR=1 \
    -e KAFKA_TRANSACTION_STATE_LOG_MIN_ISR=1 \
    -e KAFKA_GROUP_INITIAL_REBALANCE_DELAY_MS=0 \
    "$KAFKA_IMAGE" >/dev/null
  if [[ $sr == sr ]]; then
    docker run -d --name "$STACK_NAME-sr" --network "$STACK_NET" -p "127.0.0.1:$SR_PORT:8081" \
      -e SCHEMA_REGISTRY_HOST_NAME=sr \
      -e SCHEMA_REGISTRY_LISTENERS=http://0.0.0.0:8081 \
      -e SCHEMA_REGISTRY_KAFKASTORE_BOOTSTRAP_SERVERS="PLAINTEXT://$STACK_NAME:9094" \
      "$SR_IMAGE" >/dev/null
  fi

  # The advertised listener is the host port, which does not exist
  # inside the container, so wait on the log line instead of a client
  # inside it.
  echo "== wait for kafka on :$KAFKA_PORT"
  local _
  for _ in $(seq 120); do
    docker logs "$STACK_NAME" 2>&1 | grep -q "Kafka Server started" && break
    sleep 0.5
  done
  docker logs "$STACK_NAME" 2>&1 | grep -q "Kafka Server started" || { docker logs "$STACK_NAME" >&2; return 1; }
  export KAFKA_BROKERS="127.0.0.1:$KAFKA_PORT"

  if [[ $sr == sr ]]; then
    export SR_URL="http://127.0.0.1:$SR_PORT"
    echo "== wait for schema registry on :$SR_PORT"
    for _ in $(seq 240); do
      curl -fsS "$SR_URL/subjects" >/dev/null 2>&1 && break
      sleep 0.5
    done
    curl -fsS "$SR_URL/subjects" >/dev/null || { docker logs "$STACK_NAME-sr" >&2; return 1; }
  fi
}
