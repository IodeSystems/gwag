#!/usr/bin/env bash
# Run the @gwag/events tests, JetStream ones included, against a NATS
# server in docker. Without docker, point NATS_URL at a server that
# has JetStream on and run `pnpm test`.
#
#   ts/events/test.sh
#
# NATS_IMAGE overrides the image, NATS_PORT the host port (default 14223).
set -euo pipefail
cd "$(dirname "$0")"

NATS_IMAGE="${NATS_IMAGE:-nats:2.10-alpine}"
NATS_PORT="${NATS_PORT:-14223}"
NAME="gwag-ts-events-nats-$$"

pnpm install --frozen-lockfile >/dev/null
pnpm -s typecheck

if [[ -n "${NATS_URL:-}" ]]; then
  pnpm -s test
  exit
fi

cleanup() { docker rm -f "$NAME" >/dev/null 2>&1 || true; }
trap cleanup EXIT INT TERM
docker run -d --name "$NAME" -p "127.0.0.1:$NATS_PORT:4222" "$NATS_IMAGE" -js >/dev/null
for _ in $(seq 100); do
  (exec 3<>"/dev/tcp/127.0.0.1/$NATS_PORT") 2>/dev/null && break
  sleep 0.1
done
sleep 0.5
NATS_URL="nats://127.0.0.1:$NATS_PORT" pnpm -s test
