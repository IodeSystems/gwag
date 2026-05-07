#!/usr/bin/env bash
# Boot the bench stack: 1 gateway (n1) + 1 greeter backend (g1) +
# Prometheus + Grafana via docker-compose. Idempotent — re-running
# without down.sh first will probably wedge on already-listening
# ports; use bench/down.sh to clean.
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck disable=SC1091
source "$SCRIPT_DIR/lib.sh"

echo "==> building binaries"
build_binaries

echo "==> starting gateway n1"
"$SCRIPT_DIR/scale.sh" add-gateway

echo "==> starting backend g1"
"$SCRIPT_DIR/scale.sh" add-backend greeter --version v1

echo "==> starting prometheus + grafana (docker compose)"
(cd "$SCRIPT_DIR" && docker compose up -d)

LAN_IP="$(hostname -I 2>/dev/null | awk '{print $1}')"
[[ -z $LAN_IP ]] && LAN_IP=localhost

cat <<EOF

bench up.

  Gateway     http://${LAN_IP}:18080/api/graphql
  Schema      http://${LAN_IP}:18080/api/schema/graphql
  Metrics     http://${LAN_IP}:18080/api/metrics
  Prometheus  http://${LAN_IP}:19090
  Grafana     http://${LAN_IP}:3001  (admin / admin)

Try:
  bench/scale.sh status
  bench/scale.sh add-gateway
  bench/scale.sh add-backend greeter --version v2
  bench/.run/bin/traffic --target http://${LAN_IP}:18080/api/graphql --rps 200 --duration 30s
  bench/down.sh
EOF
