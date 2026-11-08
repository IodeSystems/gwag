#!/usr/bin/env bash
# Check examples/events: regenerate gen/ with `gwag gen`, fail if it
# differs from what is committed, then run the typed-API tests and the
# JetStream example.
#
#   examples/events/check.sh          # Go, JetStream (embedded NATS)
#   KAFKA=1 examples/events/check.sh  # also go-kafka/test.sh and ts-kafka/test.sh (docker)
#
# Needs go, and node >= 24 with pnpm for the TypeScript part. The
# generators are the ones the projects pin (protoc-gen-go from the root
# go.mod tool directive, protoc-gen-es from node_modules/.bin); nothing
# is taken from PATH. The TypeScript JetStream tests and the Go /
# TypeScript interop run need docker (a NATS container); without it
# they are skipped and the script says so.
set -euo pipefail
cd "$(dirname "$0")"
ROOT="$(cd ../.. && pwd)"
cd "$ROOT"

# gw/cmd/gwag embeds ui/dist; seed it the way bin/build does.
if [[ -z "$(ls -A ui/dist 2>/dev/null)" ]]; then
  mkdir -p ui/dist
  cp ui/fallback/index.html ui/dist/index.html
fi

echo "== gwag gen --lang go"
go run ./gw/cmd/gwag gen --lang go --out examples/events/gen \
  -I examples/events/proto --opt paths=source_relative \
  orders/v1/orders.proto

fail=0
if ! git diff --quiet -- examples/events/gen || [[ -n "$(git ls-files --others --exclude-standard -- examples/events/gen)" ]]; then
  echo "FAIL regenerated output differs from committed:" >&2
  git status --short -- examples/events/gen >&2
  fail=1
fi

echo "== vet + typed-API tests"
go vet ./examples/events/...
go test -count=1 ./examples/events/...

echo "== go-jetstream"
out="$(go run ./examples/events/go-jetstream)"
echo "$out"
for want in \
  "subscribes    orders.v1.OrderEvents as billing" \
  "OrderEvents   o-1001 created by ann" \
  "OrderEvents   o-1002 cancelled: out of stock" \
  "Payments      eu/card captured 4200 for o-1001" \
  "Payments      eu/card refunded 1500: cancelled" \
  "OrderShipped  o-1001 via ups" \
  "Audit         o-1002.refund: support refund"; do
  if grep -qF -- "$want" <<<"$out"; then
    echo "ok   $want"
  else
    echo "FAIL missing '$want'" >&2
    fail=1
  fi
done

echo "== gwag gen --lang ts"
pnpm --dir ts/events install --frozen-lockfile >/dev/null
pnpm --dir examples/events/ts install --frozen-lockfile >/dev/null
ts/events/gen.sh >/dev/null
examples/events/ts/gen.sh >/dev/null
ts_gen="ts/events/src/gen examples/events/ts/src/gen"
if ! git diff --quiet -- $ts_gen || [[ -n "$(git ls-files --others --exclude-standard -- $ts_gen)" ]]; then
  echo "FAIL regenerated TypeScript differs from committed:" >&2
  git status --short -- $ts_gen >&2
  fail=1
fi

echo "== typecheck + tests (ts/events, examples/events/ts)"
pnpm --dir ts/events typecheck || fail=1
pnpm --dir ts/events test || fail=1
pnpm --dir examples/events/ts typecheck || fail=1
pnpm --dir examples/events/ts test || fail=1

if command -v docker >/dev/null 2>&1 && docker info >/dev/null 2>&1; then
  echo "== ts/events JetStream tests (docker)"
  ts/events/test.sh || fail=1
  echo "== Go <-> TypeScript interop (docker)"
  examples/events/interop.sh || fail=1
else
  echo "SKIP JetStream tests and Go <-> TypeScript interop: docker is not available"
fi

if [[ "${KAFKA:-}" == 1 ]]; then
  echo "== go-kafka (docker)"
  examples/events/go-kafka/test.sh || fail=1
  echo "== ts-kafka (docker)"
  examples/events/ts-kafka/test.sh || fail=1
fi

if [[ $fail == 0 ]]; then echo "PASS"; else echo "FAILED" >&2; exit 1; fi
