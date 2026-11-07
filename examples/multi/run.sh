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

# Persist the admin boot token so the MCP worked example (cmd/mcp-demo)
# can read it without scraping logs. Path matches what the demo
# defaults to. Token survives across restarts of run.sh.
ADMIN_DATA_DIR="${ADMIN_DATA_DIR:-/tmp/gwag-multi}"
mkdir -p "$ADMIN_DATA_DIR"

# --insecure-subscribe lets the worked Subscriptions tutorial in the
# repo README run end-to-end with no sign step. Production deployments
# must pair --subscribe-secret with the SignSubscriptionToken flow —
# see README §HMAC channel auth.
#
# --admin-data-dir persists the admin token so cmd/mcp-demo (and any
# other admin client) can read <dir>/admin-token directly.
go run ./cmd/gateway --insecure-subscribe --admin-data-dir "$ADMIN_DATA_DIR" &
pids+=($!)

# Give the control plane a moment to come up before services try to register.
sleep 1

go run ./cmd/greeter &
pids+=($!)

go run ./cmd/library &
pids+=($!)

wait
