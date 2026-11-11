#!/usr/bin/env bash
# Drives headless Chrome (CDP, 127.0.0.1 only) and the gwag CLI through
# a real OIDC sign-in against Dex (default) or Keycloak.
#   ./e2e.sh               # Dex: email and domain rules
#   IDP=keycloak ./e2e.sh  # Keycloak: group rules, group changes, claim flag
# Needs docker, google-chrome, node 22+. Screenshots and the gateway log
# land in .run/. Skips (exit 0) without docker or Chrome.
set -euo pipefail
cd "$(dirname "$0")"
command -v docker >/dev/null && docker info >/dev/null 2>&1 || { echo "SKIP: docker not available"; exit 0; }
command -v google-chrome >/dev/null || { echo "SKIP: google-chrome not found"; exit 0; }
source ./stack.sh
trap stack_stop EXIT INT TERM
stack_start
export SSO_RUN=$RUN GWAG_BIN=$ROOT/bin/.run/gwag-$(go env GOOS)-$(go env GOARCH) IDP
SSO_PHASE=main node e2e.mjs
if [[ "$IDP" == keycloak ]]; then
	# Second claim, full group paths, rules in the other order, claim flag set.
	gw_stop
	RULES=(--sso-rule group:/gwag-devs=dev --sso-rule group:/gwag-admins=admin)
	gw_start --sso-groups-claim roles_groups
	SSO_PHASE=claim node e2e.mjs
fi
