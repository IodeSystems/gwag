#!/usr/bin/env bash
# Start a single-node Kafka in docker, run the @gwag/events-kafka tests
# against it, stop it.
#
#   ts/events-kafka/test.sh
#
# Needs docker, node >= 24 and pnpm. KAFKA_PORT (default 19092) is the
# host port; KAFKA_IMAGE overrides the pinned image. Without docker,
# point KAFKA_BROKERS at a broker and run `pnpm test`.
set -euo pipefail
cd "$(dirname "$0")"
source ../../examples/events/kafka-stack.sh

pnpm install --frozen-lockfile >/dev/null
pnpm -s typecheck
stack_up
pnpm -s test
echo PASS
