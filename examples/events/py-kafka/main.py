"""The generated order bus over Kafka: the same typed calls as
../py/main.py, another Backend.

    KAFKA_BROKERS=localhost:9092 uv run python main.py

./test.sh starts apache/kafka in docker and runs this and the backend
tests. The generated code and the sample handlers come from ../py.
"""

import asyncio
import os
import sys
import time
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent / "py"))

from app import listen_all, publish_all  # noqa: E402
from proto_gen.orders.v1.orders_events import OrdersBus  # noqa: E402

from gwag_events_kafka import KafkaBackend  # noqa: E402

WANT = 7


async def main() -> int:
    brokers = os.environ.get("KAFKA_BROKERS")
    if not brokers:
        print("set KAFKA_BROKERS (e.g. localhost:9092)", file=sys.stderr)
        return 2
    backend = KafkaBackend(brokers)
    bus = OrdersBus(backend)
    # A fresh group per run: a new group starts at new messages, so a
    # rerun against a long-lived broker does not see the last run's.
    group = f"example-{int(time.time())}"
    lines: list[str] = []
    stops = await listen_all(bus, group, lines.append)

    for s in bus.subscriptions():
        print(f"subscribes    {s.channel} as {s.group}")
    await publish_all(bus)

    deadline = time.monotonic() + 30
    while len(lines) < WANT:
        if time.monotonic() > deadline:
            print(f"timed out waiting for deliveries: got {len(lines)} of {WANT}", file=sys.stderr)
            return 1
        await asyncio.sleep(0.05)
    for line in sorted(lines):
        print(line)
    for stop in stops:
        await stop()
    await backend.close()
    return 0


if __name__ == "__main__":
    sys.exit(asyncio.run(main()))
