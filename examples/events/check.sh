#!/usr/bin/env bash
# Check examples/events: regenerate gen/ with `gwag gen`, fail if it
# differs from what is committed, then run the typed-API tests and the
# JetStream example.
#
#   examples/events/check.sh          # Go, JetStream (embedded NATS)
#   KAFKA=1 examples/events/check.sh  # also go-kafka/test.sh (docker)
#
# Needs go. protoc-gen-go comes from the root go.mod tool directive;
# nothing is taken from PATH.
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

if [[ "${KAFKA:-}" == 1 ]]; then
  echo "== go-kafka (docker)"
  examples/events/go-kafka/test.sh || fail=1
fi

if [[ $fail == 0 ]]; then echo "PASS"; else echo "FAILED" >&2; exit 1; fi
