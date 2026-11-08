"""A service that registers itself and waits: the program
e2e/registration/check.sh runs against a real gateway.

    python tests/fixture_service.py --gateway HOST:PORT --namespace NS --proto FILE \\
        [--import PATH=FILE]... [--subscribe CHANNEL=GROUP]... [--key KEY] [--ttl SECONDS]

Prints "REGISTERED <id>" once the gateway says ok, then runs until
SIGTERM or SIGINT, which deregister it.
"""

from __future__ import annotations

import argparse
import asyncio
import signal
from pathlib import Path

from gwag_registration import Options, Service, Subscription, register


async def main() -> None:
    p = argparse.ArgumentParser()
    p.add_argument("--gateway", required=True)
    p.add_argument("--namespace", required=True)
    p.add_argument("--proto", required=True)
    p.add_argument("--import", dest="imports", action="append", default=[])
    p.add_argument("--subscribe", action="append", default=[])
    p.add_argument("--key")
    p.add_argument("--ttl", type=int, default=6)
    a = p.parse_args()

    imports: dict[str, str | bytes] = {k: Path(v).read_text() for k, v in (kv.split("=", 1) for kv in a.imports)}
    subs = [Subscription(*kv.split("=", 1)) for kv in a.subscribe]
    reg = await register(
        Options(
            gateway=a.gateway,
            service_addr="127.0.0.1:1",  # nothing calls it
            instance_id=f"{a.namespace}-py",
            ttl_seconds=a.ttl,
            application_key=a.key,
            ready_poll_interval=0.2,
            deregister_on_signal=(signal.SIGTERM, signal.SIGINT),
            services=[
                Service(
                    namespace=a.namespace,
                    version="v1",
                    proto_source=Path(a.proto).read_text(),
                    proto_imports=imports,
                    subscribes=subs,
                )
            ],
        )
    )
    print(f"REGISTERED {reg.id}", flush=True)
    await asyncio.Event().wait()  # until a signal


asyncio.run(main())
