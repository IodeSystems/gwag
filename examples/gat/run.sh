#!/usr/bin/env bash
# Boots the Go server and the Vite UI side-by-side. Ctrl+C cleans up.
set -euo pipefail
cd "$(dirname "$0")"

go run ./server &
SERVER_PID=$!
trap "kill $SERVER_PID 2>/dev/null || true" EXIT

cd ui
if [ ! -d node_modules ]; then
  pnpm install
fi
if [ ! -d src/gql ]; then
  # Give the server a beat to come up before introspecting.
  sleep 2
  pnpm gen
fi
pnpm dev
