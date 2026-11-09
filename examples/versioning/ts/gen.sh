#!/usr/bin/env bash
# Regenerate src/gen/ from the frozen OpenAPI snapshots in ../api, with
# the openapi-typescript this package pins. No server needed.
set -euo pipefail
cd "$(dirname "$0")"
rm -rf src/gen
mkdir -p src/gen
for v in v1 v2; do
  pnpm exec openapi-typescript "../api/orders/$v/openapi.json" -o "src/gen/orders-$v.ts" >/dev/null
done
