#!/usr/bin/env bash
# Boot the bench stack: 1 gateway (n1) + 1 greeter backend (g1) +
# Prometheus + Grafana via docker-compose. Idempotent — re-running
# without down.sh first will probably wedge on already-listening
# ports; use bench/down.sh to clean.
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck disable=SC1091
source "$SCRIPT_DIR/lib.sh"

echo "==> ensuring binaries"
build_binaries_if_missing

# Refuse to start on top of leftover state — would silently allocate
# n2/n3 etc. and produce a confusing tree. The user almost always
# wants down.sh first.
existing=$(find "$GW_DIR" "$BE_DIR" -name '*.env' 2>/dev/null | head -n 1)
if [[ -n $existing ]]; then
    echo "leftover state in $RUN_DIR/{gateways,backends}; bench/down.sh --purge first" >&2
    exit 1
fi

echo "==> starting gateway n1"
"$SCRIPT_DIR/scale.sh" add-gateway

echo "==> starting backend g1"
"$SCRIPT_DIR/scale.sh" add-backend greeter --version v1

echo "==> starting prometheus + grafana (docker compose)"
(cd "$SCRIPT_DIR" && docker compose up -d)

LAN_IP="$(hostname -I 2>/dev/null | awk '{print $1}')"
[[ -z $LAN_IP ]] && LAN_IP=localhost

UI_HINT=""
if [[ ! -d "$REPO_DIR/ui/dist/assets" || -z "$(ls -A "$REPO_DIR/ui/dist/assets" 2>/dev/null)" ]]; then
    UI_HINT='
NOTE: the gateway binary embeds whatever was last in ui/dist/. The
committed placeholder is enough for /api/* and traffic gen, but the
admin SPA (services / peers / schema / events panels) only renders
in a browser if you build the UI:
  cd ui && pnpm install && pnpm run build
Then re-run bench/up.sh so the gateway re-embeds the fresh dist.'
fi

ADMIN_TOKEN_HINT="(missing — gateway log $LOG_DIR/n1.log will have it)"
ADMIN_TOKEN_FILE="$NATS_DIR/n1/admin-token"
if [[ -r $ADMIN_TOKEN_FILE ]]; then
    ADMIN_TOKEN_HINT="$(cat "$ADMIN_TOKEN_FILE")"
fi

cat <<EOF

bench up.

  Gateway     http://${LAN_IP}:18080/api/graphql
  Schema      http://${LAN_IP}:18080/api/schema/graphql
  Metrics     http://${LAN_IP}:18080/api/metrics
  Prometheus  http://${LAN_IP}:19090
  Grafana     http://${LAN_IP}:3001  (admin / admin)
  Admin UI    http://${LAN_IP}:18080/  (paste token below in Settings)

  Admin token  ${ADMIN_TOKEN_HINT}

Quick sanity check (one greeter dispatch):
  curl -s -X POST http://${LAN_IP}:18080/api/graphql \\
    -H 'Content-Type: application/json' \\
    -d '{"query":"{ greeter { hello(name:\\"world\\") { greeting } } }"}'

Scale + load:
  bin/bench status
  bin/bench scale add-gateway
  bin/bench scale add-backend greeter --version v2
  bin/bench traffic --target http://${LAN_IP}:18080/api/graphql --rps 200 --duration 30s

Tear down:
  bin/bench down              # purges .run/ by default
  bin/bench down --no-purge   # keep .run/ for a faster re-up${UI_HINT}
EOF
