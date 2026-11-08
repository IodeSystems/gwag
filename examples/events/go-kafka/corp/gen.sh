#!/usr/bin/env bash
# Write gen/ for ./main.go: import subject corp-orders-value from the
# registry at SR_URL (read-only copy and lock in imported/), and the
# shadow copy in shadow/shop/v1/order.proto. ../test.sh runs it.
set -euo pipefail
cd "$(dirname "$0")"
ROOT="$(cd ../../../.. && pwd)"
M=github.com/iodesystems/gwag/v2/examples/events/go-kafka/corp
gwag() { (cd "$ROOT" && go run ./gw/cmd/gwag "$@"); }
HERE="$(pwd)"

rm -rf gen imported
gwag gen --registry "$SR_URL" --subject corp-orders-value --import-dir "$HERE/imported" \
  --lang go --out "$HERE" --opt "module=$M" \
  --opt "Mregistry/corp-orders-value.proto=$M/gen/corpordersv1;corpordersv1" \
  --opt "Mcorp/common/v1/money.proto=$M/gen/corpcommonv1;corpcommonv1"
gwag gen --lang go --out "$HERE" -I "$HERE/shadow" --opt "module=$M" shop/v1/order.proto
