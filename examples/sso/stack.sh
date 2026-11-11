# Sourced by run.sh and e2e.sh: start Dex and the gateway, stop both.
# Gateway 127.0.0.1:28500 (control plane 28501, NATS 28502/28503), Dex 28556.
HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
ROOT=$(cd "$HERE/../.." && pwd)
RUN=$HERE/.run
GW=http://127.0.0.1:28500
DEX=http://127.0.0.1:28556/dex
GWPID=

stack_stop() {
	[[ -n "$GWPID" ]] && kill "$GWPID" 2>/dev/null && wait "$GWPID" 2>/dev/null
	GWPID=
	(cd "$HERE" && docker compose down -v --remove-orphans >/dev/null 2>&1) || true
}

stack_start() {
	mkdir -p "$RUN"
	rm -rf "$RUN/data" "$RUN/gw.log"
	(cd "$HERE" && docker compose up -d dex >/dev/null) || return 1
	for _ in $(seq 60); do
		curl -fs "$DEX/.well-known/openid-configuration" >/dev/null && break
		sleep 0.5
	done
	curl -fs "$DEX/.well-known/openid-configuration" >/dev/null || { echo "dex did not come up" >&2; return 1; }
	[[ -d "$ROOT/ui/dist" ]] || "$ROOT/bin/build" --no-go >/dev/null
	REBUILD=1 GWAG_SSO_CLIENT_SECRET=sso-example-client-secret \
		"$ROOT/bin/gwag" up --addr 127.0.0.1:28500 --control-plane 127.0.0.1:28501 \
		--nats-listen 127.0.0.1:28502 --nats-cluster 127.0.0.1:28503 \
		--data-dir "$RUN/data" --no-context-write \
		--sso-issuer "$DEX" --sso-client-id gwag \
		--sso-redirect-url "$GW/auth/callback" \
		--sso-rule email:admin@example.test=admin \
		--sso-rule domain:example.test=dev \
		>"$RUN/gw.log" 2>&1 &
	GWPID=$!
	for _ in $(seq 120); do
		curl -fs "$GW/auth/whoami" >/dev/null && return 0
		kill -0 "$GWPID" 2>/dev/null || { tail -20 "$RUN/gw.log" >&2; return 1; }
		sleep 0.5
	done
	tail -20 "$RUN/gw.log" >&2
	return 1
}
