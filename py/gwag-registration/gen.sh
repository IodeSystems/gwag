#!/usr/bin/env bash
# Regenerate src/gwag_registration/gen/: the Python stubs for the control
# plane (gw/proto/controlplane/v1/control.proto): messages, the
# ControlPlaneStub the client calls through, and ControlPlaneBase the
# tests serve. Run from the repo:
#
#   py/gwag-registration/gen.sh
#
# Uses the betterproto2 compiler this project pins (uv.lock), via
# `gwag gen`. Needs uv and go. A gateway with the Schema Registry on
# serves the same file (subject gateway.controlplane.v1.ControlPlane), so
# a service outside this repo can generate the same code from its gateway.
set -euo pipefail
cd "$(dirname "$0")"
ROOT="$(cd ../.. && pwd)"

if [[ -z "$(ls -A "$ROOT/ui/dist" 2>/dev/null)" ]]; then
  mkdir -p "$ROOT/ui/dist"
  cp "$ROOT/ui/fallback/index.html" "$ROOT/ui/dist/index.html"
fi
uv sync --locked --quiet
rm -rf src/gwag_registration/gen
(cd "$ROOT" && go run ./gw/cmd/gwag gen --lang python --module-dir py/gwag-registration \
  --out py/gwag-registration/src/gwag_registration/gen -I . \
  --opt client_generation=async --opt server_generation=async \
  gw/proto/controlplane/v1/control.proto >/dev/null)
