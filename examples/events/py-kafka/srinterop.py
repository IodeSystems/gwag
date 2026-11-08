"""Publish and read the order channels over Kafka with Schema Registry
framing, for ./test.sh to check against Confluent's own serializer and
deserializer and against the Go runtime. The Python twin of
../go-kafka/srinterop.

    srinterop.py produce SHIPPED_TOPIC EVENTS_TOPIC [lookup|latest]
    srinterop.py consume SHIPPED_TOPIC EVENTS_TOPIC N

KAFKA_BROKERS and SR_URL name the broker and the registry. produce
registers orders.proto under <topic>-value on first publish (own);
with lookup it only looks the schema up, with latest it frames with the
subject's latest schema (use_latest).
consume reads from the start of both topics and prints N lines; the
records need not carry gwag headers.
"""

import asyncio
import os
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent / "py"))

from gwag_events import Message, SubscribeOptions, message_from  # noqa: E402
from gwag_events_kafka import KafkaBackend, Topic, topic_table  # noqa: E402
from gwag_events_schemaregistry import HEADER_SCHEMA_ID, Client, Config, Serde  # noqa: E402
from proto_gen.orders.v1 import OrderCancelled, OrderCreated, OrderShipped  # noqa: E402
from proto_gen.orders.v1.orders_events import (  # noqa: E402
    OrderEventsSubject,
    OrdersBus,
    OrderShippedSubject,
)


async def main() -> int:
    if len(sys.argv) < 4:
        print("usage: srinterop.py produce|consume SHIPPED_TOPIC EVENTS_TOPIC [N]", file=sys.stderr)
        return 2
    cmd, shipped, evs = sys.argv[1:4]
    backend = KafkaBackend(
        os.environ["KAFKA_BROKERS"],
        partitions=1,
        topics=topic_table(
            {
                # A producer that is not gwag keys records by order id and
                # sends no Gwag-* headers; subject rebuilds the subject.
                "orders.v1.OrderShipped": Topic(
                    name=shipped, subject=lambda r: f"events.orders.{(r.key or b'').decode()}.shipped"
                ),
                "orders.v1.OrderEvents": Topic(
                    name=evs, subject=lambda r: f"events.orders.{(r.key or b'').decode()}.lifecycle"
                ),
            }
        ),
    )
    client = Client(Config(url=os.environ["SR_URL"]))
    try:
        if cmd == "produce":
            mode = sys.argv[4] if len(sys.argv) > 4 else ""
            if mode not in ("", "lookup", "latest"):
                print(f"produce mode {mode!r}: want lookup or latest", file=sys.stderr)
                return 2
            serde = Serde(client, auto_register=mode == "", use_latest=mode == "latest")
            bus = OrdersBus(backend, serde=serde)
            await bus.publish_order_shipped(
                OrderShippedSubject(order_id="o-1001"), OrderShipped(order_id="o-1001", carrier="ups")
            )
            await bus.publish_order_events(
                OrderEventsSubject(order_id="o-1001"), OrderCreated(order_id="o-1001", customer="ann")
            )
            print("produced 2")
            return 0
        if cmd == "consume":
            return await consume(OrdersBus(backend, serde=Serde(client)), int(sys.argv[4]))
        print(f"unknown command {cmd!r}", file=sys.stderr)
        return 2
    finally:
        await backend.close()
        await client.aclose()


def _type_source(m: Message) -> str:
    if m.headers.get("Gwag-Channel"):
        return "Gwag-Type header"
    return "schema id " + m.headers.get(HEADER_SCHEMA_ID, "")


async def consume(bus: OrdersBus, n: int) -> int:
    lines: list[str] = []

    def got(line: str) -> None:
        lines.append(line)

    async def shipped(s: OrderShippedSubject, m: OrderShipped) -> None:
        raw = message_from()
        assert raw is not None
        got(
            f"OrderShipped {m.order_id} via {m.carrier} (key {raw.key.decode()}, "
            f"schema id {raw.headers.get(HEADER_SCHEMA_ID, '')}, gwag headers {bool(raw.headers.get('Gwag-Channel'))})"
        )

    async def created(s: OrderEventsSubject, m: OrderCreated) -> None:
        raw = message_from()
        assert raw is not None
        got(f"OrderCreated {m.order_id} by {m.customer} (type from {_type_source(raw)})")

    async def cancelled(s: OrderEventsSubject, m: OrderCancelled) -> None:
        raw = message_from()
        assert raw is not None
        got(f"OrderCancelled {m.order_id}: {m.reason} (type from {_type_source(raw)})")

    async def unknown(s: OrderEventsSubject, m: Message) -> None:
        got(f"Unknown {m.type}")

    opts = SubscribeOptions(deliver_all=True)
    await bus.on_order_shipped("srinterop-py", shipped, options=opts)
    await bus.on_order_events(
        "srinterop-py", order_created=created, order_cancelled=cancelled, unknown=unknown, options=opts
    )
    try:
        async with asyncio.timeout(60):
            while len(lines) < n:
                await asyncio.sleep(0.05)
    except TimeoutError:
        print(f"timed out; got {sorted(lines)!r}", file=sys.stderr)
        return 1
    for line in sorted(lines):
        print(line)
    return 0


if __name__ == "__main__":
    sys.exit(asyncio.run(main()))
