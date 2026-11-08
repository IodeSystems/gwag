#!/usr/bin/env bash
# Start a single-node Kafka in docker, run the @gwag/events-kafka tests
# (ts/events-kafka) and this example against it, stop it.
#
#   examples/events/ts-kafka/test.sh
#
# Needs docker, node >= 24 and pnpm. KAFKA_PORT (default 19092) is the
# host port; KAFKA_IMAGE overrides the pinned image.
set -euo pipefail
cd "$(dirname "$0")"
source ../kafka-stack.sh

pnpm install --frozen-lockfile >/dev/null
pnpm -s typecheck
(cd ../../../ts/events-kafka && pnpm install --frozen-lockfile >/dev/null && pnpm -s typecheck)
stack_up
echo "== backend tests (ts/events-kafka)"
(cd ../../../ts/events-kafka && pnpm -s test)
echo "== example"
out="$(pnpm -s start)"
echo "$out"
for want in "OrderEvents   o-1001 created by ann" "OrderEvents   o-1002 cancelled: out of stock" \
  "Payments      eu/card captured 4200 for o-1001" "OrderShipped  o-1001 via ups"; do
  grep -qF -- "$want" <<<"$out" || { echo "FAIL missing '$want'" >&2; exit 1; }
done
echo PASS
