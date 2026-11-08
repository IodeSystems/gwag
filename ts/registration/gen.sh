#!/usr/bin/env bash
# Regenerate src/gen/: the TypeScript stubs for the control plane
# (gw/proto/controlplane/v1/control.proto), including the ControlPlane
# service descriptor the client calls through. Run from the repo:
#
#   pnpm --dir ts/registration gen
#
# Uses the protoc-gen-es this package pins (node_modules/.bin), via
# `gwag gen`. Needs go. A gateway with the Schema Registry on serves the
# same file (subject gateway.controlplane.v1.ControlPlane), so a service
# outside this repo can generate the same code from its gateway.
set -euo pipefail
cd "$(dirname "$0")"
ROOT="$(cd ../.. && pwd)"

if [[ -z "$(ls -A "$ROOT/ui/dist" 2>/dev/null)" ]]; then
  mkdir -p "$ROOT/ui/dist"
  cp "$ROOT/ui/fallback/index.html" "$ROOT/ui/dist/index.html"
fi
pnpm install --frozen-lockfile >/dev/null
rm -rf src/gen
(cd "$ROOT" && go run ./gw/cmd/gwag gen --lang ts --module-dir ts/registration \
  --out ts/registration/src/gen -I . \
  --opt target=ts --opt import_extension=ts --opt erasable_syntax=true \
  gw/proto/controlplane/v1/control.proto)
