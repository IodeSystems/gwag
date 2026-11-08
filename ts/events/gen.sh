#!/usr/bin/env bash
# Regenerate src/gen/: the TypeScript code for gwag's own option file
# (gw/proto/ps/v1/options.proto). Generated event code imports it as
# @gwag/events/proto/gw/proto/ps/v1/options_pb.js. Run from the repo:
#
#   pnpm --dir ts/events gen
#
# The map_imports opt switches off the default mapping of gw/proto/ps/ to
# this package, which would make the file import itself.
#
# Uses the protoc-gen-es this package pins (node_modules/.bin), via
# `gwag gen`. Needs go.
set -euo pipefail
cd "$(dirname "$0")"
ROOT="$(cd ../.. && pwd)"

if [[ -z "$(ls -A "$ROOT/ui/dist" 2>/dev/null)" ]]; then
  mkdir -p "$ROOT/ui/dist"
  cp "$ROOT/ui/fallback/index.html" "$ROOT/ui/dist/index.html"
fi
pnpm install --frozen-lockfile >/dev/null
rm -rf src/gen
(cd "$ROOT" && go run ./gw/cmd/gwag gen --lang ts --module-dir ts/events \
  --out ts/events/src/gen -I . \
  --opt target=ts --opt import_extension=ts --opt erasable_syntax=true \
  --opt map_imports=none/:@gwag/events/none \
  gw/proto/ps/v1/options.proto)
