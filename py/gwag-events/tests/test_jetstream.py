"""Runs against NATS with JetStream at $NATS_URL, e.g.

    docker run --rm -p 4222:4222 nats:2.10-alpine -js
    NATS_URL=nats://127.0.0.1:4222 uv run pytest
"""

import os

import nats
import pytest
from nats.aio.client import Client as NATS
from nats.aio.msg import Msg

from gwag_events import (
    Channel,
    Handler,
    JetStreamBackend,
    Message,
    Stop,
    SubscribeOptions,
    dlq_subject,
    testing,
)
from gwag_events.testing import Harness

URL = os.environ.get("NATS_URL", "")
pytestmark = pytest.mark.skipif(not URL, reason="set NATS_URL to a NATS server with JetStream")


class _Backend:
    """JetStreamBackend plus a core-NATS listener on the dead-letter subjects."""

    def __init__(self, nc: NATS, inner: JetStreamBackend) -> None:
        self.nc, self.inner = nc, inner
        self.dlq: list[Message] = []

    async def publish(self, channel: Channel, message: Message) -> None:
        await self.inner.publish(channel, message)

    async def subscribe(self, channel: Channel, group: str, opts: SubscribeOptions, handler: Handler) -> Stop:
        return await self.inner.subscribe(channel, group, opts, handler)

    async def close(self) -> None:
        await self.inner.close()
        await self.nc.close()


async def _new() -> _Backend:
    nc = await nats.connect(URL)
    b = _Backend(nc, JetStreamBackend(nc, prefetch=1))

    async def on_dlq(msg: Msg) -> None:
        b.dlq.append(Message(subject=msg.subject, data=msg.data, headers=dict(msg.headers or {})))

    await nc.subscribe("gwag.dlq.>", cb=on_dlq)
    return b


async def _dead_letters(b: object, stream: str, n: int) -> list[Message]:
    assert isinstance(b, _Backend)
    want = dlq_subject(stream)
    await testing.wait_for(f"{n} dead letters", lambda: sum(m.subject == want for m in b.dlq) >= n)
    return [m for m in b.dlq if m.subject == want]


@pytest.mark.parametrize("name", list(testing.CHECKS))
async def test_conformance(name: str) -> None:
    await testing.CHECKS[name](Harness(new=_new, dead_letters=_dead_letters))


async def _noop(m: Message) -> None:
    return None


async def test_stream_config_and_consumer_reuse() -> None:
    nc = await nats.connect(URL)
    try:
        b = JetStreamBackend(nc, prefetch=1)
        ch = testing.channel()
        stop = await b.subscribe(ch, "g", SubscribeOptions(max_deliver=7), _noop)
        js = nc.jetstream()
        info = await js.stream_info(ch.stream)
        cfg = info.config
        assert cfg.subjects == [ch.pattern.wildcard()]
        assert cfg.storage == "file"
        assert cfg.retention == "limits"
        assert cfg.max_age == 7 * 24 * 3600 and cfg.duplicate_window == 120 and cfg.num_replicas == 1
        ci = await js.consumer_info(ch.stream, "g")
        assert ci.config.deliver_policy == "new"
        assert ci.config.max_deliver == 7
        await stop()
        # A second subscriber with other options uses the group as it is.
        stop = await b.subscribe(ch, "g", SubscribeOptions(max_deliver=2, deliver_all=True), _noop)
        ci = await js.consumer_info(ch.stream, "g")
        assert ci.config.max_deliver == 7
        assert ci.config.deliver_policy == "new"
        await stop()
        await b.close()
    finally:
        await nc.close()
