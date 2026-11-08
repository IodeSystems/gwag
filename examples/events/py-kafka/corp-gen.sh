#!/usr/bin/env bash
# Write gen_corp/ and gen_shop/ for ./corp.py: import subject
# corp-orders-value from the registry at SR_URL (read-only copy and lock
# in imported/), and gwag's shadow copy of it. ./test.sh runs it.
set -euo pipefail
cd "$(dirname "$0")"
HERE="$(pwd)"
ROOT="$(cd ../../.. && pwd)"
gwag() { (cd "$ROOT" && go run ./gw/cmd/gwag "$@"); }

rm -rf gen_corp gen_shop imported
gwag gen --registry "$SR_URL" --subject corp-orders-value --import-dir "$HERE/imported" \
  --lang python --module-dir "$HERE" --out "$HERE/gen_corp" --opt paths=source_relative
gwag gen --lang python --module-dir "$HERE" --out "$HERE/gen_shop" \
  -I "$HERE/../go-kafka/corp/shadow" --opt paths=source_relative shop/v1/order.proto
