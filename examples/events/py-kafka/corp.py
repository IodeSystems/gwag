"""Read topic corp-orders, which another team produces with Confluent's
serializer (subject corp-orders-value, message corp.orders.v1.Order).
Two ways, one per row of docs/events-kafka.md "Who produces the topic?". The
Python twin of ../go-kafka/corp.

    corp.py import TOPIC   # typed code from a pinned copy of the subject
    corp.py shadow TOPIC   # typed code from gwag's own copy, ../go-kafka/corp/shadow/shop/v1/order.proto

gen_corp/ and gen_shop/ are written by ./corp-gen.sh (./test.sh runs
it), not committed. KAFKA_BROKERS and SR_URL name the broker and the
registry.
"""

import asyncio
import os
import sys

from gwag_events import SubscribeOptions, message_from
from gwag_events_kafka import KafkaBackend, Topic, topic_table
from gwag_events_schemaregistry import HEADER_SCHEMA_ID, Client, Config, Serde

WANT = 2


async def main() -> int:
    if len(sys.argv) != 3 or sys.argv[1] not in ("import", "shadow"):
        print("usage: corp.py import|shadow TOPIC", file=sys.stderr)
        return 2
    mode, topic = sys.argv[1:3]
    brokers = os.environ["KAFKA_BROKERS"]
    client = Client(Config(url=os.environ["SR_URL"]))
    lines: list[str] = []
    backend: KafkaBackend
    if mode == "import":
        from gen_corp.corp.orders.v1 import Order
        from gen_corp.corp.orders.v1.corp_orders_value_events import CorpOrdersValueBus, OrderSubject

        # The channel key is <package>.<message name>; the topic is the
        # team's, so the mapper names it.
        backend = KafkaBackend(brokers, topics=topic_table({"corp.orders.v1.Order": Topic(name=topic)}))
        bus = CorpOrdersValueBus(backend, serde=Serde(client))

        async def on_order(s: OrderSubject, m: Order) -> None:
            raw = message_from()
            assert raw is not None
            total = m.total
            lines.append(
                f"import {raw.type} {m.order_id} total {total.cents if total else 0} {total.currency if total else ''} "
                f"status {m.status.name} skus {m.skus} (key {raw.key.decode()}, schema id {raw.headers[HEADER_SCHEMA_ID]})"
            )

        await bus.on_order("corp-import-py", on_order, options=SubscribeOptions(deliver_all=True))
    else:
        from gen_shop.shop.v1 import CorpOrder
        from gen_shop.shop.v1.order_events import CorpOrdersSubject, OrderBus

        backend = KafkaBackend(brokers, topics=topic_table({"shop.v1.CorpOrders": Topic(name=topic)}))
        # Records are framed as corp.orders.v1.Order; the handler takes
        # them as the copy.
        serde = Serde(client, type_names={"corp.orders.v1.Order": "shop.v1.CorpOrder"})
        shop = OrderBus(backend, serde=serde)

        async def on_corp(s: CorpOrdersSubject, m: CorpOrder) -> None:
            raw = message_from()
            assert raw is not None
            lines.append(
                f"shadow {raw.type} {m.order_id} total {m.total.cents if m.total else 0} (key {raw.key.decode()})"
            )

        await shop.on_corp_orders("corp-shadow-py", on_corp, options=SubscribeOptions(deliver_all=True))
    try:
        async with asyncio.timeout(60):
            while len(lines) < WANT:
                await asyncio.sleep(0.05)
    except TimeoutError:
        print(f"timed out; got {sorted(lines)!r}", file=sys.stderr)
        return 1
    finally:
        await backend.close()
        await client.aclose()
    for line in sorted(lines):
        print(line)
    return 0


if __name__ == "__main__":
    sys.exit(asyncio.run(main()))
