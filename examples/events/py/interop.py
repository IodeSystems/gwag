"""Python half of the Go/Python interop test (tests/test_interop.py).
examples/events/go-interop is the Go half; the two take the same flags
and print the same lines.

    interop.py listen  --group G [--prefetch N] [--sleep-ms N]
    interop.py publish [--bulk N] [--prefix P] [--no-canonical]

listen subscribes to every channel, prints READY, then one line per
delivery until stdin closes, then DONE. publish sends one message of
each shape plus N OrderShipped messages "o-bulk-<prefix>-<i>".
NATS_URL is required.
"""

import argparse
import asyncio
import os
import sys

import nats
from gwag_events import JetStreamBackend

from app import listen_all, publish_all
from proto_gen.orders.v1 import OrderShipped
from proto_gen.orders.v1.orders_events import OrdersBus, OrderShippedSubject


def emit(line: str) -> None:
    print(line, flush=True)


async def listen(bus: OrdersBus, args: argparse.Namespace) -> None:
    stops = await listen_all(bus, args.group, emit, sleep=args.sleep_ms / 1000)
    emit("READY")
    await asyncio.get_running_loop().run_in_executor(None, sys.stdin.read)
    for stop in stops:
        await stop()
    emit("DONE")


async def publish(bus: OrdersBus, args: argparse.Namespace) -> None:
    if not args.no_canonical:
        await publish_all(bus)
    for i in range(args.bulk):
        oid = f"o-bulk-{args.prefix}-{i}"
        await bus.publish_order_shipped(OrderShippedSubject(order_id=oid), OrderShipped(order_id=oid, carrier="ups"))
    emit("DONE")


async def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = ap.add_subparsers(dest="cmd", required=True)
    ls = sub.add_parser("listen")
    ls.add_argument("--group", required=True)
    ls.add_argument("--prefetch", type=int, default=16)
    ls.add_argument("--sleep-ms", type=int, default=0)
    pb = sub.add_parser("publish")
    pb.add_argument("--bulk", type=int, default=0)
    pb.add_argument("--prefix", default="py")
    pb.add_argument("--no-canonical", action="store_true")
    pb.add_argument("--prefetch", type=int, default=16)
    args = ap.parse_args()

    url = os.environ.get("NATS_URL")
    if not url:
        print("set NATS_URL", file=sys.stderr)
        return 2
    nc = await nats.connect(url)
    backend = JetStreamBackend(nc, prefetch=args.prefetch)
    bus = OrdersBus(backend)
    if args.cmd == "listen":
        await listen(bus, args)
    else:
        await publish(bus, args)
    await backend.close()
    await nc.close()
    return 0


if __name__ == "__main__":
    sys.exit(asyncio.run(main()))
