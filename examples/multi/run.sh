#!/usr/bin/env bash
# Run the gateway and both services. Ctrl-C cleans them all up.
set -euo pipefail
cd "$(dirname "$0")"

pids=()
cleanup() {
  for pid in "${pids[@]}"; do
    kill "$pid" 2>/dev/null || true
  done
  wait 2>/dev/null || true
}
trap cleanup EXIT INT TERM

go run ./cmd/gateway &
pids+=($!)

# Give the control plane a moment to come up before services try to register.
sleep 1

go run ./cmd/greeter &
pids+=($!)

go run ./cmd/library &
pids+=($!)

wait
