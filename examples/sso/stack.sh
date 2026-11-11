# Sourced by run.sh and e2e.sh: start the IdP and the gateway, stop both.
# IDP=dex (default) or IDP=keycloak.
# Gateway 127.0.0.1:28500 (control plane 28501, NATS 28502/28503),
# Dex 28556, Keycloak 28557.
HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
ROOT=$(cd "$HERE/../.." && pwd)
RUN=$HERE/.run
GW=http://127.0.0.1:28500
IDP=${IDP:-dex}
GWPID=
export KC_ADMIN_USER=kcadmin KC_ADMIN_PASS=kc-admin-pass-4h8w # fixture, 127.0.0.1 only

case "$IDP" in
dex)
	ISSUER=http://127.0.0.1:28556/dex
	RULES=(--sso-rule email:admin@example.test=admin --sso-rule domain:example.test=dev)
	;;
keycloak)
	ISSUER=http://127.0.0.1:28557/realms/gwag
	# Groups alone decide; first match wins, so a member of both is admin.
	RULES=(--sso-rule group:gwag-admins=admin --sso-rule group:gwag-devs=dev)
	;;
*) echo "IDP must be dex or keycloak" >&2; return 1 ;;
esac

gw_stop() {
	[[ -n "$GWPID" ]] && kill "$GWPID" 2>/dev/null && wait "$GWPID" 2>/dev/null
	GWPID=
}

stack_stop() {
	gw_stop
	(cd "$HERE" && docker compose down -v --remove-orphans >/dev/null 2>&1) || true
}

# gw_start [extra gwag flags...]; the rules come from $RULES.
# Data dir is kept across restarts (same session key); stack_start wipes it.
gw_start() {
	REBUILD=1 GWAG_SSO_CLIENT_SECRET=sso-example-client-secret \
		"$ROOT/bin/gwag" up --addr 127.0.0.1:28500 --control-plane 127.0.0.1:28501 \
		--nats-listen 127.0.0.1:28502 --nats-cluster 127.0.0.1:28503 \
		--data-dir "$RUN/data" --no-context-write \
		--sso-issuer "$ISSUER" --sso-client-id gwag \
		--sso-redirect-url "$GW/auth/callback" \
		"${RULES[@]}" "$@" \
		>>"$RUN/gw.log" 2>&1 &
	GWPID=$!
	for _ in $(seq 120); do
		curl -fs "$GW/auth/whoami" >/dev/null && return 0
		kill -0 "$GWPID" 2>/dev/null || { tail -20 "$RUN/gw.log" >&2; return 1; }
		sleep 0.5
	done
	tail -20 "$RUN/gw.log" >&2
	return 1
}

stack_start() {
	mkdir -p "$RUN"
	rm -rf "$RUN/data" "$RUN/gw.log"
	(cd "$HERE" && docker compose up -d "$IDP" >/dev/null) || return 1
	for _ in $(seq 240); do
		curl -fs "$ISSUER/.well-known/openid-configuration" >/dev/null && break
		sleep 0.5
	done
	curl -fs "$ISSUER/.well-known/openid-configuration" >/dev/null || { echo "$IDP did not come up" >&2; return 1; }
	[[ -d "$ROOT/ui/dist" ]] || "$ROOT/bin/build" --no-go >/dev/null
	gw_start "$@"
}
