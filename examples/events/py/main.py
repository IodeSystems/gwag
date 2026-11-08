"""Publishes and handles the sample order events over NATS JetStream
with the generated Python bus.

    NATS_URL=nats://127.0.0.1:4222 uv run python main.py

Needs a NATS server with JetStream (docker run --rm -p 4222:4222
nats:2.10-alpine -js), or a gwag cluster's NATS client port (gwag up:
14222). Output matches examples/events/go-jetstream.
"""

import asyncio
import os
import sys
import time

import nats
from gwag_events import JetStreamBackend

from app import listen_all, publish_all
from proto_gen.orders.v1.orders_events import OrdersBus

WANT = 7


async def main() -> int:
    url = os.environ.get("NATS_URL")
    if not url:
        print("set NATS_URL to a NATS server with JetStream, e.g. nats://127.0.0.1:4222", file=sys.stderr)
        return 2
    nc = await nats.connect(url)
    backend = JetStreamBackend(nc)
    bus = OrdersBus(backend)
    # A fresh group per run: a new group starts at new messages, so a
    # rerun on a long-lived server does not see the last run's.
    group = f"example-{int(time.time())}"
    lines: list[str] = []
    stops = await listen_all(bus, group, lines.append)

    for s in bus.subscriptions():
        print(f"subscribes    {s.channel} as {s.group}")
    await publish_all(bus)

    deadline = time.monotonic() + 10
    while len(lines) < WANT:
        if time.monotonic() > deadline:
            print(f"timed out waiting for deliveries: got {len(lines)} of {WANT}", file=sys.stderr)
            return 1
        await asyncio.sleep(0.01)
    for line in sorted(lines):
        print(line)
    for stop in stops:
        await stop()
    await backend.close()
    await nc.close()
    return 0


if __name__ == "__main__":
    sys.exit(asyncio.run(main()))
