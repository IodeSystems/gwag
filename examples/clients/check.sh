#!/usr/bin/env bash
# End-to-end check for examples/clients.
#
# Boots the examples/multi gateway plus greeter and library (the same
# three processes examples/multi/run.sh starts) on high ports, then
# regenerates both clients from the gateway's exports, runs them, and
# asserts their output. Stops everything on exit.
#
#   examples/clients/check.sh
#
# Needs: go, protoc + protoc-gen-go + protoc-gen-go-grpc (.bin/ or
# ~/go/bin), node >= 24, pnpm, curl. Override ports with HTTP_PORT,
# CP_PORT, GREETER_PORT, LIBRARY_PORT, NATS_PORT.
set -euo pipefail
cd "$(dirname "$0")"
HERE="$(pwd)"
ROOT="$(cd ../.. && pwd)"

HTTP_PORT="${HTTP_PORT:-18480}"
CP_PORT="${CP_PORT:-50480}"
GREETER_PORT="${GREETER_PORT:-50481}"
LIBRARY_PORT="${LIBRARY_PORT:-50482}"
NATS_PORT="${NATS_PORT:-18422}"

for p in "$HTTP_PORT" "$CP_PORT" "$GREETER_PORT" "$LIBRARY_PORT" "$NATS_PORT"; do
  if (exec 3<>"/dev/tcp/127.0.0.1/$p") 2>/dev/null; then
    echo "port $p is in use; set HTTP_PORT / CP_PORT / GREETER_PORT / LIBRARY_PORT / NATS_PORT" >&2
    exit 1
  fi
done

RUN="$(mktemp -d "${TMPDIR:-/tmp}/gwag-clients.XXXXXX")"
pids=()
cleanup() {
  for pid in "${pids[@]:-}"; do
    [[ -n "$pid" ]] && kill "$pid" 2>/dev/null || true
  done
  wait 2>/dev/null || true
  rm -rf "$RUN"
}
trap cleanup EXIT INT TERM

# ui/embed.go embeds ui/dist; seed it the way bin/build does.
if [[ -z "$(ls -A "$ROOT/ui/dist" 2>/dev/null)" ]]; then
  mkdir -p "$ROOT/ui/dist"
  cp "$ROOT/ui/fallback/index.html" "$ROOT/ui/dist/index.html"
fi

echo "== build gateway, greeter, library"
(cd "$ROOT" && for c in gateway greeter library; do
  go build -o "$RUN/$c" "./examples/multi/cmd/$c"
done)

echo "== boot on :$HTTP_PORT (graphql/connect/ingress) and :$CP_PORT (grpc)"
mkdir -p "$RUN/admin"
"$RUN/gateway" --http ":$HTTP_PORT" --control-plane ":$CP_PORT" \
  --nats-listen ":$NATS_PORT" --admin-data-dir "$RUN/admin" \
  --shutdown-delay 0 >"$RUN/gateway.log" 2>&1 &
pids+=($!)

for _ in $(seq 50); do
  curl -fsS "http://localhost:$HTTP_PORT/api/health" >/dev/null 2>&1 && break
  sleep 0.2
done
curl -fsS "http://localhost:$HTTP_PORT/api/health" >/dev/null || { cat "$RUN/gateway.log" >&2; exit 1; }

# The services read protos/ relative to examples/multi.
(cd "$ROOT/examples/multi" && exec "$RUN/greeter" --addr ":$GREETER_PORT" \
  --advertise "localhost:$GREETER_PORT" --gateway "localhost:$CP_PORT" \
  --nats "" >"$RUN/greeter.log" 2>&1) &
pids+=($!)
(cd "$ROOT/examples/multi" && exec "$RUN/library" --addr ":$LIBRARY_PORT" \
  --advertise "localhost:$LIBRARY_PORT" --gateway "localhost:$CP_PORT" \
  >"$RUN/library.log" 2>&1) &
pids+=($!)

export GATEWAY="http://localhost:$HTTP_PORT"

# Wait until both services have registered (schema exports need no token).
for svc in greeter library; do
  for _ in $(seq 50); do
    n="$(curl -fsS "$GATEWAY/api/schema/proto?service=$svc" 2>/dev/null | wc -c)"
    [[ "$n" -gt 0 ]] && break
    sleep 0.2
  done
  [[ "$n" -gt 0 ]] || { echo "$svc did not register" >&2; exit 1; }
done

fail=0
expect() { # name, output, fixed-string
  if grep -qF -- "$3" <<<"$2"; then
    echo "ok   $1: $3"
  else
    echo "FAIL $1: missing '$3'" >&2
    fail=1
  fi
}

echo "== go-proto: gen + run"
"$HERE/go-proto/gen.sh" >/dev/null
go_out="$(cd "$ROOT" && GWAG_GRPC="localhost:$CP_PORT" GWAG_CONNECT="$GATEWAY/api/connect" \
  go run ./examples/clients/go-proto)"
echo "$go_out"
expect go-proto "$go_out" "grpc    greeter.Hello -> Hello, grpc!"
expect go-proto "$go_out" "grpc    library.ListBooks -> 3 books"
expect go-proto "$go_out" "connect greeter.Hello -> Hello, connect!"
expect go-proto "$go_out" "connect library.GetBook(1) -> Go Programming by Alan Donovan"

echo "== ts-openapi: install + gen + typecheck + run"
(cd "$HERE/ts-openapi" && pnpm install --frozen-lockfile >/dev/null && ./gen.sh >/dev/null && pnpm -s typecheck)
ts_out="$(cd "$HERE/ts-openapi" && GWAG_INGRESS="$GATEWAY/api/ingress" pnpm -s start)"
echo "$ts_out"
expect ts-openapi "$ts_out" "greeter.Hello -> Hello, openapi!"
expect ts-openapi "$ts_out" "library.ListBooks -> 3 books"
expect ts-openapi "$ts_out" "library.GetBook(1) -> Go Programming by Alan Donovan"

# Regenerating must not change committed output.
if ! git -C "$ROOT" diff --quiet -- examples/clients; then
  echo "FAIL regenerated output differs from committed:" >&2
  git -C "$ROOT" diff --stat -- examples/clients >&2
  fail=1
fi

if [[ $fail == 0 ]]; then echo "PASS"; else echo "FAILED" >&2; exit 1; fi
