#!/usr/bin/env bash
# Gateway with OIDC sign-in against a local Dex or Keycloak (docker).
# Open the URL, click "Sign in with SSO". Ctrl-C stops both.
#   ./run.sh                # Dex
#   IDP=keycloak ./run.sh   # Keycloak (about 30 s to start)
set -euo pipefail
cd "$(dirname "$0")"
source ./stack.sh
trap stack_stop EXIT INT TERM
stack_start
if [[ "$IDP" == keycloak ]]; then
	echo "UI: $GW   (grp-admin / grp-dev / grp-both / grp-none @corp.test; passwords in keycloak-realm.json)"
	echo "Keycloak admin console: http://127.0.0.1:28557 (user $KC_ADMIN_USER, password in stack.sh)"
else
	echo "UI: $GW   (admin@example.test / dev@example.test / stranger@other.test; passwords in dex.yaml)"
fi
echo "log: $RUN/gw.log"
wait "$GWPID"
