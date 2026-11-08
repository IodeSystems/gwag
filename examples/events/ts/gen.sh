#!/usr/bin/env bash
# Regenerate src/gen/ from ../proto with `gwag gen --lang ts`. The
# base generator is the protoc-gen-es this package pins
# (node_modules/.bin); nothing is taken from PATH.
#
#   pnpm --dir examples/events/ts gen
set -euo pipefail
cd "$(dirname "$0")"
ROOT="$(cd ../../.. && pwd)"

if [[ -z "$(ls -A "$ROOT/ui/dist" 2>/dev/null)" ]]; then
  mkdir -p "$ROOT/ui/dist"
  cp "$ROOT/ui/fallback/index.html" "$ROOT/ui/dist/index.html"
fi
pnpm install --frozen-lockfile >/dev/null
rm -rf src/gen
(cd "$ROOT" && go run ./gw/cmd/gwag gen --lang ts --module-dir examples/events/ts \
  --out examples/events/ts/src/gen -I examples/events/proto \
  --opt target=ts --opt import_extension=ts --opt erasable_syntax=true \
  orders/v1/orders.proto)
