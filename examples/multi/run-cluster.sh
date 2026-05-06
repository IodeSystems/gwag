#!/usr/bin/env bash
# Run a 3-node gateway cluster with embedded NATS + JetStream KV
# registry. Boots three gateways that form a cluster, then registers
# two greeters at v1 + one greeter at v2 across different gateways to
# show that any gateway can dispatch to any registered service.
#
# Ctrl-C cleans up.
set -euo pipefail
cd "$(dirname "$0")"

DATA_ROOT=${DATA_ROOT:-/tmp/multi-cluster}
rm -rf "$DATA_ROOT"
mkdir -p "$DATA_ROOT"/{n1,n2,n3}

pids=()
cleanup() {
  echo "stopping..."
  for pid in "${pids[@]}"; do
    kill "$pid" 2>/dev/null || true
  done
  wait 2>/dev/null || true
}
trap cleanup EXIT INT TERM

# Build once so logs are clean.
go build -o "$DATA_ROOT/gateway" ./cmd/gateway
go build -o "$DATA_ROOT/greeter" ./cmd/greeter

# Three gateways forming a cluster. Each lists at least one peer so
# NATS is in cluster mode (standalone JetStream cannot scale up later).
"$DATA_ROOT/gateway" \
  --node-name n1 \
  --http :18080 --control-plane :50090 \
  --nats-listen :14222 --nats-cluster :14248 \
  --nats-data "$DATA_ROOT/n1" \
  --nats-peer 127.0.0.1:14249 --nats-peer 127.0.0.1:14250 \
  > "$DATA_ROOT/n1.log" 2>&1 &
pids+=($!)

"$DATA_ROOT/gateway" \
  --node-name n2 \
  --http :18081 --control-plane :50091 \
  --nats-listen :14223 --nats-cluster :14249 \
  --nats-data "$DATA_ROOT/n2" \
  --nats-peer 127.0.0.1:14248 --nats-peer 127.0.0.1:14250 \
  > "$DATA_ROOT/n2.log" 2>&1 &
pids+=($!)

"$DATA_ROOT/gateway" \
  --node-name n3 \
  --http :18082 --control-plane :50092 \
  --nats-listen :14224 --nats-cluster :14250 \
  --nats-data "$DATA_ROOT/n3" \
  --nats-peer 127.0.0.1:14248 --nats-peer 127.0.0.1:14249 \
  > "$DATA_ROOT/n3.log" 2>&1 &
pids+=($!)

# Wait for all three control planes to bind.
for port in 50090 50091 50092; do
  while ! nc -z 127.0.0.1 "$port" 2>/dev/null; do sleep 0.5; done
done
# Give the cluster a moment to form a meta leader and replicate buckets.
sleep 5

# Three greeters: two at v1 (registered on different gateways) and one
# at v2. The reconciler on every gateway sees them all via KV watch.
"$DATA_ROOT/greeter" --addr :15101 --advertise localhost:15101 \
  --gateway localhost:50090 --version v1 > "$DATA_ROOT/g1.log" 2>&1 &
pids+=($!)
"$DATA_ROOT/greeter" --addr :15102 --advertise localhost:15102 \
  --gateway localhost:50091 --version v1 > "$DATA_ROOT/g2.log" 2>&1 &
pids+=($!)
"$DATA_ROOT/greeter" --addr :15103 --advertise localhost:15103 \
  --gateway localhost:50092 --version v2 > "$DATA_ROOT/g3.log" 2>&1 &
pids+=($!)

cat <<EOF

Cluster up. GraphQL endpoints:
  n1 → http://localhost:18080/graphql
  n2 → http://localhost:18081/graphql
  n3 → http://localhost:18082/graphql

Try the same query against each — every gateway dispatches to the
greeter registered with any peer:

  curl -s -X POST http://localhost:18080/graphql \\
    -H 'Content-Type: application/json' \\
    -d '{"query":"{ greeter { hello(name:\"world\") { greeting } } }"}'

Logs at $DATA_ROOT. Ctrl-C to stop everything.
EOF

wait
