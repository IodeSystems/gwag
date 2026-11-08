"""Confluent's Python serializer and deserializer against gwag's framing.

    uv run --locked python confluent_check.py consume SHIPPED_TOPIC EVENTS_TOPIC
    uv run --locked python confluent_check.py produce SHIPPED_TOPIC EVENTS_TOPIC
    uv run --locked python confluent_check.py produce-corp TOPIC

KAFKA_BROKERS and SR_URL name the broker and the registry. The order
protos are compiled at start with grpcio-tools into a temp directory.

consume reads one OrderShipped and one OrderCreated with
ProtobufDeserializer and prints them. produce writes one of each with
ProtobufSerializer (auto-registering the schema, references under the
import path), keyed by order id, without Gwag-* headers. produce-corp
does the same with proto/corp/orders/v1/order.proto, a proto gwag does
not own: two corp.orders.v1.Order records.
"""

import os
import sys
import tempfile
from pathlib import Path

from grpc_tools import protoc

HERE = Path(__file__).resolve().parent
ROOT = HERE.parents[3]


def load_protos():
    out = tempfile.mkdtemp(prefix="gwag-confluent-")
    wkt = str(Path(protoc.__file__).parent / "_proto")
    for f in ("gw/proto/ps/v1/options.proto", "orders/v1/orders.proto",
              "corp/common/v1/money.proto", "corp/orders/v1/order.proto"):
        rc = protoc.main(["protoc", f"-I{ROOT}", f"-I{ROOT}/examples/events/proto", f"-I{HERE}/proto",
                          f"-I{wkt}", f"--python_out={out}", f])
        if rc != 0:
            sys.exit(f"protoc {f}: exit {rc}")
    sys.path.insert(0, out)
    from orders.v1 import orders_pb2  # noqa: E402
    from corp.orders.v1 import order_pb2  # noqa: E402

    return orders_pb2, order_pb2


def main() -> None:
    cmd = sys.argv[1]
    orders_pb2, corp_pb2 = load_protos()
    from confluent_kafka import DeserializingConsumer, SerializingProducer
    from confluent_kafka.schema_registry import SchemaRegistryClient
    from confluent_kafka.schema_registry.protobuf import ProtobufDeserializer, ProtobufSerializer
    from confluent_kafka.serialization import StringDeserializer, StringSerializer

    brokers = os.environ["KAFKA_BROKERS"]
    sr = SchemaRegistryClient({"url": os.environ["SR_URL"]})

    if cmd == "produce-corp":
        topic = sys.argv[2]
        p = SerializingProducer({
            "bootstrap.servers": brokers,
            "key.serializer": StringSerializer("utf_8"),
            "value.serializer": ProtobufSerializer(corp_pb2.Order, sr, {"use.deprecated.format": False}),
        })
        for oid, cents, skus in (("o-3001", 4200, ["sku-1"]), ("o-3002", 999, ["sku-2", "sku-3"])):
            p.produce(topic, key=oid, value=corp_pb2.Order(
                order_id=oid, total={"cents": cents, "currency": "EUR"}, status=corp_pb2.Order.PLACED, skus=skus))
        p.flush()
        print("confluent produced 2 corp.orders.v1.Order")
        return
    shipped_topic, events_topic = sys.argv[2:4]
    if cmd == "consume":
        for topic, cls in ((shipped_topic, orders_pb2.OrderShipped), (events_topic, orders_pb2.OrderCreated)):
            c = DeserializingConsumer({
                "bootstrap.servers": brokers,
                "group.id": "confluent-check-" + topic,
                "auto.offset.reset": "earliest",
                "key.deserializer": StringDeserializer("utf_8"),
                "value.deserializer": ProtobufDeserializer(cls, {"use.deprecated.format": False}, sr),
            })
            c.subscribe([topic])
            msg = None
            for _ in range(120):
                msg = c.poll(0.5)
                if msg is not None:
                    break
            c.close()
            if msg is None:
                sys.exit(f"no record on {topic}")
            if msg.error():
                sys.exit(f"{topic}: {msg.error()}")
            v = msg.value()
            headers = dict(msg.headers() or [])
            print(f"confluent {cls.DESCRIPTOR.full_name}: {' '.join(f'{f.name}={val}' for f, val in v.ListFields())}"
                  f" (key {msg.key()}, Gwag-Type {headers.get('Gwag-Type', b'').decode()})")
    elif cmd == "produce":
        p = SerializingProducer({
            "bootstrap.servers": brokers,
            "key.serializer": StringSerializer("utf_8"),
            "value.serializer": ProtobufSerializer(orders_pb2.OrderShipped, sr, {"use.deprecated.format": False}),
        })
        p.produce(shipped_topic, key="o-2001", value=orders_pb2.OrderShipped(order_id="o-2001", carrier="dhl"))
        p.flush()
        p = SerializingProducer({
            "bootstrap.servers": brokers,
            "key.serializer": StringSerializer("utf_8"),
            "value.serializer": ProtobufSerializer(orders_pb2.OrderCreated, sr, {"use.deprecated.format": False}),
        })
        p.produce(events_topic, key="o-2001", value=orders_pb2.OrderCreated(order_id="o-2001", customer="cy"))
        p.flush()
        print("confluent produced 2")
    else:
        sys.exit(f"unknown command {cmd}")


if __name__ == "__main__":
    main()
