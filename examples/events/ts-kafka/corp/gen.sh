#!/usr/bin/env bash
# Write gen/ for ./main.ts: import subject corp-orders-value from the
# registry at SR_URL (read-only copy and lock in imported/), and the
# shadow copy in ../../go-kafka/corp/shadow/shop/v1/order.proto (the
# Go example's). ../sr-test.sh runs it.
set -euo pipefail
cd "$(dirname "$0")"
ROOT="$(cd ../../../.. && pwd)"
HERE="$(pwd)"
OPTS=(--opt target=ts --opt import_extension=ts --opt erasable_syntax=true)
gwag() { (cd "$ROOT" && go run ./gw/cmd/gwag "$@"); }

rm -rf gen imported
gwag gen --registry "$SR_URL" --subject corp-orders-value --import-dir "$HERE/imported" \
  --lang ts --module-dir examples/events/ts-kafka --out "$HERE/gen" "${OPTS[@]}"
gwag gen --lang ts --module-dir examples/events/ts-kafka --out "$HERE/gen" \
  -I examples/events/go-kafka/corp/shadow "${OPTS[@]}" shop/v1/order.proto
