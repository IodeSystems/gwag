"""Behaviour checks every Backend must pass: fan-out across groups,
work sharing in a group, redelivery, dead-lettering, dedup, deliver-all.
Mirrors events/eventstest in Go. Backend packages call `CHECKS` from
their tests, one pytest case per entry."""

from __future__ import annotations

import asyncio
import itertools
from collections.abc import Awaitable, Callable
from dataclasses import dataclass

from .channel import Channel, dlq_subject
from .core import Core, message_from
from .types import (
    HEADER_CHANNEL,
    HEADER_ERROR,
    Backend,
    Message,
    SubscribeOptions,
    new_id,
)

TYPE_NAME = "test.Text"


class Text:
    """A stand-in proto message: its body is the utf-8 text."""

    def __init__(self, text: str) -> None:
        self.text = text

    def __bytes__(self) -> bytes:
        return self.text.encode()


def decode(m: Message) -> str:
    return m.data.decode()


@dataclass
class Harness:
    # A fresh backend. Each call must not see messages from earlier
    # calls (use a fresh server or the unique channel keys below).
    new: Callable[[], Awaitable[Backend]]
    # Waits until n dead letters for stream exist and returns them.
    dead_letters: Callable[[Backend, str, int], Awaitable[list[Message]]]
    # Wait after subscribe returns until the subscriber is receiving.
    settle: float = 0.0
    # Skip the dedup check, for backends without message-id dedup (Kafka).
    no_dedup: bool = False
    # Adjusts the options of every subscribe (a backend's own mode,
    # such as Kafka's retry_topic).
    adjust: Callable[[SubscribeOptions], SubscribeOptions] | None = None


_seq = itertools.count(1)
_run = new_id()[:8]


def channel() -> Channel:
    """A channel with a key unique to this process, so runs against a
    long-lived broker do not see each other's messages."""
    n = next(_seq)
    return Channel.new(f"test.v1.C{_run}_{n}", f"test.c{_run}_{n}.{{id}}")


async def wait_for(what: str, cond: Callable[[], bool], timeout: float = 10.0) -> None:
    loop = asyncio.get_running_loop()
    deadline = loop.time() + timeout
    while not cond():
        if loop.time() > deadline:
            raise AssertionError(f"timed out waiting for {what}")
        await asyncio.sleep(0.005)


async def _pub(
    c: Core, ch: Channel, key: str, body: str, *, msg_id: str | None = None, headers: dict[str, str] | None = None
) -> None:
    await c.publish(ch, [key], Text(body), TYPE_NAME, msg_id=msg_id, headers=headers)


async def _sub(
    h: Harness,
    c: Core,
    ch: Channel,
    group: str,
    fn: Callable[[list[str], Message], Awaitable[None]],
    options: SubscribeOptions | None = None,
) -> Callable[[], Awaitable[None]]:
    opts = options or SubscribeOptions()
    stop = await c.subscribe(ch, group, fn, h.adjust(opts) if h.adjust else opts)
    await asyncio.sleep(h.settle)
    return stop


async def fan_out(h: Harness) -> None:
    b = await h.new()
    try:
        c = Core(b)
        ch = channel()
        a: list[str] = []
        bb: list[str] = []

        async def ha(segs: list[str], m: Message) -> None:
            assert message_from() is m, "message_from() is not the delivered message"
            assert m.type == TYPE_NAME and m.headers.get(HEADER_CHANNEL) == ch.key, m.headers
            a.append(f"{segs[0]}={decode(m)}")

        async def hb(segs: list[str], m: Message) -> None:
            bb.append(f"{segs[0]}={decode(m)}")

        s1 = await _sub(h, c, ch, "a", ha)
        s2 = await _sub(h, c, ch, "b", hb)
        await _pub(c, ch, "x1", "hello")
        await wait_for("both groups", lambda: len(a) == 1 and len(bb) == 1)
        assert a == ["x1=hello"] and bb == ["x1=hello"], (a, bb)
        subs = [(s.channel, s.group) for s in c.subscriptions()]
        assert subs == [(ch.key, "a"), (ch.key, "b")], subs
        await s1()
        await s2()
    finally:
        await b.close()


async def group_shares(h: Harness) -> None:
    b = await h.new()
    try:
        c = Core(b)
        ch = channel()
        one: list[str] = []
        two: list[str] = []

        def slow(g: list[str]) -> Callable[[list[str], Message], Awaitable[None]]:
            async def fn(segs: list[str], m: Message) -> None:
                await asyncio.sleep(0.005)
                g.append(decode(m))

            return fn

        s1 = await _sub(h, c, ch, "shared", slow(one))
        s2 = await _sub(h, c, ch, "shared", slow(two))
        n = 40
        # Distinct subjects, so a backend that partitions by subject
        # (Kafka) spreads them.
        for i in range(n):
            await _pub(c, ch, f"k{i}", str(i))
        await wait_for("all messages", lambda: len(one) + len(two) >= n)
        await asyncio.sleep(0.05)
        assert len(one) + len(two) == n, f"delivered {len(one) + len(two)}, want {n} (each message once per group)"
        assert one and two, f"work not shared: {len(one)} / {len(two)}"
        assert len(c.subscriptions()) == 1, c.subscriptions()
        await s1()
        await s2()
    finally:
        await b.close()


async def redelivery(h: Harness) -> None:
    b = await h.new()
    try:
        c = Core(b)
        ch = channel()
        deliveries: list[int] = []

        async def fn(segs: list[str], m: Message) -> None:
            deliveries.append(m.delivery)
            if m.delivery < 3:
                raise RuntimeError("not yet")

        stop = await _sub(h, c, ch, "r", fn, SubscribeOptions(backoff=(0.01,), max_deliver=5))
        await _pub(c, ch, "k", "retry me")
        await wait_for("third delivery", lambda: len(deliveries) >= 3)
        await asyncio.sleep(0.05)
        assert deliveries == [1, 2, 3], deliveries
        await stop()
    finally:
        await b.close()


async def dead_letter(h: Harness) -> None:
    b = await h.new()
    try:
        c = Core(b)
        ch = channel()
        calls: list[int] = []

        async def fn(segs: list[str], m: Message) -> None:
            calls.append(m.delivery)
            raise RuntimeError("boom")

        stop = await _sub(h, c, ch, "d", fn, SubscribeOptions(backoff=(0.005,), max_deliver=2))
        await _pub(c, ch, "k", "doomed", headers={"X-Trace": "t1"})
        m = (await h.dead_letters(b, ch.stream, 1))[0]
        assert m.subject == dlq_subject(ch.stream), m.subject
        assert m.headers.get(HEADER_ERROR) == "boom", m.headers
        assert m.headers.get("X-Trace") == "t1" and m.type == TYPE_NAME, m.headers
        assert decode(m) == "doomed", decode(m)
        await asyncio.sleep(0.05)
        assert len(calls) == 2, f"handler calls = {len(calls)}, want 2"
        await stop()
    finally:
        await b.close()


async def dedup(h: Harness) -> None:
    b = await h.new()
    try:
        c = Core(b)
        ch = channel()
        got: list[str] = []

        async def fn(segs: list[str], m: Message) -> None:
            got.append(f"{decode(m)}/{m.id}")

        stop = await _sub(h, c, ch, "u", fn)
        await _pub(c, ch, "k", "first", msg_id="same")
        await _pub(c, ch, "k", "second", msg_id="same")
        await _pub(c, ch, "k", "third")
        await wait_for("two messages", lambda: len(got) >= 2)
        await asyncio.sleep(0.05)
        assert len(got) == 2 and got[0] == "first/same", got
        await stop()
    finally:
        await b.close()


async def deliver_all(h: Harness) -> None:
    b = await h.new()
    try:
        c = Core(b)
        ch = channel()
        await _pub(c, ch, "k", "old")
        fresh: list[str] = []
        everything: list[str] = []

        async def f1(segs: list[str], m: Message) -> None:
            fresh.append(decode(m))

        async def f2(segs: list[str], m: Message) -> None:
            everything.append(decode(m))

        s1 = await _sub(h, c, ch, "new", f1)
        s2 = await _sub(h, c, ch, "all", f2, SubscribeOptions(deliver_all=True))
        await _pub(c, ch, "k", "new")
        await wait_for("deliveries", lambda: len(fresh) >= 1 and len(everything) >= 2)
        await asyncio.sleep(0.05)
        assert fresh == ["new"] and everything == ["old", "new"], (fresh, everything)
        await s1()
        await s2()
    finally:
        await b.close()


CHECKS: dict[str, Callable[[Harness], Awaitable[None]]] = {
    "fan_out_across_groups": fan_out,
    "group_shares_work": group_shares,
    "redelivery": redelivery,
    "dead_letter": dead_letter,
    "dedup": dedup,
    "deliver_all": deliver_all,
}
