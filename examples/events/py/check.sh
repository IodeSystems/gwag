#!/usr/bin/env bash
# Check the Python side of examples/events: regenerate proto_gen/ with
# `gwag gen --lang python`, fail if it differs from what is committed,
# type-check with mypy --strict, run the runtime and example tests
# (JetStream, Go<->Python interop) against an embedded NATS server.
#
#   examples/events/py/check.sh                  # embedded NATS from go-interop
#   NATS_URL=nats://127.0.0.1:4222 examples/events/py/check.sh
#   KAFKA=1 examples/events/py/check.sh          # also py-kafka (docker)
#
# Needs uv and go. Every tool is pinned in a pyproject.toml / uv.lock;
# nothing is taken from PATH except uv itself.
set -euo pipefail
cd "$(dirname "$0")"
HERE="$PWD"
ROOT="$(cd ../../.. && pwd)"

command -v uv >/dev/null || { echo "uv not found: https://docs.astral.sh/uv/" >&2; exit 1; }

# gw/cmd/gwag embeds ui/dist; seed it the way bin/build does.
if [[ -z "$(ls -A "$ROOT/ui/dist" 2>/dev/null)" ]]; then
  mkdir -p "$ROOT/ui/dist"
  cp "$ROOT/ui/fallback/index.html" "$ROOT/ui/dist/index.html"
fi

echo "== uv lock --check"
uv lock --check
(cd "$ROOT/py/gwag-events" && uv lock --check)
uv sync --locked --quiet
(cd "$ROOT/py/gwag-events" && uv sync --locked --quiet)

echo "== gwag gen --lang python"
(cd "$ROOT" && go run ./gw/cmd/gwag gen --lang python --module-dir examples/events/py \
  --out examples/events/py/proto_gen -I examples/events/proto --opt paths=source_relative \
  orders/v1/orders.proto >/dev/null)

fail=0
if ! git diff --quiet -- "$HERE/proto_gen" || [[ -n "$(git ls-files --others --exclude-standard -- "$HERE/proto_gen")" ]]; then
  echo "FAIL regenerated output differs from committed:" >&2
  git status --short -- "$HERE/proto_gen" >&2
  fail=1
fi

echo "== mypy --strict"
(cd "$ROOT/py/gwag-events" && uv run mypy src tests) || fail=1
uv run mypy || fail=1

# One NATS server for every test below, unless the caller gave one.
if [[ -z "${NATS_URL:-}" ]]; then
  tmp="$(mktemp -d)"
  (cd "$ROOT" && go build -o "$tmp/go-interop" ./examples/events/go-interop)
  mkfifo "$tmp/hold"
  "$tmp/go-interop" serve <"$tmp/hold" >"$tmp/url" &
  nats_pid=$!
  exec 9>"$tmp/hold" # closing fd 9 stops the server
  trap 'exec 9>&-; wait "$nats_pid" 2>/dev/null || true; rm -rf "$tmp"' EXIT
  for _ in $(seq 100); do [[ -s "$tmp/url" ]] && break; sleep 0.1; done
  NATS_URL="$(head -n1 "$tmp/url")"
  [[ -n "$NATS_URL" ]] || { echo "embedded NATS did not start" >&2; exit 1; }
  export NATS_URL
fi
echo "== NATS_URL=$NATS_URL"

echo "== pytest py/gwag-events (memory + JetStream)"
(cd "$ROOT/py/gwag-events" && uv run pytest -q) || fail=1

echo "== pytest examples/events/py (typed API, main.py, Go<->Python interop)"
uv run pytest -q || fail=1

if [[ "${KAFKA:-}" == 1 ]]; then
  echo "== py-kafka (docker)"
  "$HERE/../py-kafka/test.sh" || fail=1
fi

if [[ $fail == 0 ]]; then echo "PASS"; else echo "FAILED" >&2; exit 1; fi
