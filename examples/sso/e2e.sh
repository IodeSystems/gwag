#!/usr/bin/env bash
# Drives headless Chrome (CDP, 127.0.0.1 only) and the gwag CLI through
# a real OIDC sign-in against Dex. Needs docker, google-chrome, node 22+.
# Screenshots and the gateway log land in .run/. Skips (exit 0) without
# docker or Chrome.
set -euo pipefail
cd "$(dirname "$0")"
command -v docker >/dev/null && docker info >/dev/null 2>&1 || { echo "SKIP: docker not available"; exit 0; }
command -v google-chrome >/dev/null || { echo "SKIP: google-chrome not found"; exit 0; }
source ./stack.sh
trap stack_stop EXIT INT TERM
stack_start
export SSO_RUN=$RUN GWAG_BIN=$ROOT/bin/.run/gwag-$(go env GOOS)-$(go env GOARCH)
node e2e.mjs
