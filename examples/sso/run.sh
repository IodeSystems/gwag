#!/usr/bin/env bash
# Gateway with OIDC sign-in against a local Dex (docker). Open the URL,
# click "Sign in with SSO". Ctrl-C stops both.
#   ./run.sh
set -euo pipefail
cd "$(dirname "$0")"
source ./stack.sh
trap stack_stop EXIT INT TERM
stack_start
echo "UI: $GW   (admin@example.test / dev@example.test / stranger@other.test; passwords in dex.yaml)"
echo "log: $RUN/gw.log"
wait "$GWPID"
