"""Listen on every order channel over Kafka as one consumer group, with
the Schema Registry serde; print READY, then one line per delivery
until stdin closes. The e2e story e2e/s12_kafka_events_test.go runs it;
the lines are those of ../py/interop.py listen.

    uv run python listen.py --group G

KAFKA_BROKERS and SR_URL name the broker and the registry. Topics are
the backend defaults (one per channel, EV_<pkg>_<Name>).
"""

import argparse
import asyncio
import os
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent / "py"))

from app import listen_all  # noqa: E402
from proto_gen.orders.v1.orders_events import OrdersBus  # noqa: E402

from gwag_events_kafka import KafkaBackend  # noqa: E402
from gwag_events_schemaregistry import Client, Config, Serde  # noqa: E402


def emit(line: str) -> None:
    print(line, flush=True)


async def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--group", required=True)
    args = ap.parse_args()
    backend = KafkaBackend(os.environ["KAFKA_BROKERS"])
    client = Client(Config(url=os.environ["SR_URL"]))
    bus = OrdersBus(backend, serde=Serde(client))
    stops = await listen_all(bus, args.group, emit)
    emit("READY")
    await asyncio.get_running_loop().run_in_executor(None, sys.stdin.read)
    for stop in stops:
        await stop()
    await backend.close()
    await client.aclose()
    emit("DONE")
    return 0


if __name__ == "__main__":
    sys.exit(asyncio.run(main()))
