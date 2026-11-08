"""KafkaBackend against a real broker. $KAFKA_BROKERS names it;
examples/events/py-kafka/test.sh starts apache/kafka in docker and sets
it."""

import asyncio
import os
import uuid
from collections.abc import Callable

import pytest
from aiokafka import AIOKafkaConsumer, AIOKafkaProducer
from gwag_events import (
    Backend,
    Channel,
    Core,
    Message,
    SubscribeOptions,
    dlq_subject,
    new_id,
    testing,
)
from gwag_events.testing import Harness, Text

from gwag_events_kafka import (
    HEADER_SUBJECT,
    KafkaBackend,
    Topic,
    retry_in_process,
    retry_topic,
    topic_table,
)

BROKERS = os.environ.get("KAFKA_BROKERS", "")
pytestmark = pytest.mark.skipif(not BROKERS, reason="KAFKA_BROKERS not set; run examples/events/py-kafka/test.sh")


async def _new() -> Backend:
    return KafkaBackend(BROKERS)


async def _read_topic(topic: str, n: int) -> list[tuple[bytes, bytes, dict[str, str]]]:
    """n records of topic from its start: key, value, headers."""
    consumer = AIOKafkaConsumer(topic, bootstrap_servers=BROKERS, auto_offset_reset="earliest")
    await consumer.start()
    out: list[tuple[bytes, bytes, dict[str, str]]] = []
    try:
        async with asyncio.timeout(30):
            while len(out) < n:
                for records in (await consumer.getmany(timeout_ms=500)).values():
                    for r in records:
                        out.append((r.key or b"", r.value or b"", {k: v.decode() for k, v in (r.headers or [])}))
    except TimeoutError:
        raise AssertionError(f"{topic}: got {len(out)} records, want {n}") from None
    finally:
        await consumer.stop()
    return out


async def _dead_letters(b: Backend, stream: str, n: int) -> list[Message]:
    topic = dlq_subject(stream)
    return [
        Message(subject=topic, data=v, headers=h) for _, v, h in await _read_topic(topic, n)
    ]


def _harness(adjust: Callable[[SubscribeOptions], SubscribeOptions] | None = None) -> Harness:
    # settle: a second member joining a group triggers a rebalance.
    # Kafka does not drop a record because its Gwag-Msg-Id was seen, so
    # the dedup check does not apply.
    return Harness(new=_new, dead_letters=_dead_letters, settle=1.0, no_dedup=True, adjust=adjust)


@pytest.mark.parametrize("name", [n for n in testing.CHECKS if n != "dedup"])
async def test_conformance(name: str) -> None:
    await testing.CHECKS[name](_harness())


@pytest.mark.parametrize("name", [n for n in testing.CHECKS if n != "dedup"])
async def test_conformance_retry_topic(name: str) -> None:
    await testing.CHECKS[name](_harness(retry_topic))


async def test_publish_right_after_subscribe() -> None:
    """A new group's start is resolved before subscribe returns."""
    run = uuid.uuid4().hex[:8]

    async def trial(i: int) -> bool:
        b = KafkaBackend(BROKERS)
        try:
            ch = Channel.new(f"race.v1.P{run}_{i}", f"race.p{run}_{i}.{{id}}")
            got = asyncio.Event()

            async def h(m: Message) -> None:
                got.set()

            stop = await b.subscribe(ch, "g", SubscribeOptions().resolved(), h)
            await b.publish(ch, Message(subject=f"race.p{run}_{i}.x", data=b"x", id=f"m{i}"))
            try:
                await asyncio.wait_for(got.wait(), 10)
            except asyncio.TimeoutError:
                return False
            finally:
                await stop()
            return True
        finally:
            await b.close()

    ok = await asyncio.gather(*(trial(i) for i in range(8)))
    assert all(ok), f"lost on trials {[i for i, v in enumerate(ok) if not v]}"


async def test_retry_topic_does_not_block_partition() -> None:
    """A failing record does not hold up the next one on its partition,
    and its retries carry the attempt number."""
    b = KafkaBackend(BROKERS, partitions=1)
    try:
        c = Core(b)
        ch = testing.channel()
        seen: list[str] = []
        done = asyncio.Event()

        async def h(segs: list[str], m: Message) -> None:
            body = m.data.decode()
            seen.append(f"{body}#{m.delivery}")
            if body == "bad" and m.delivery < 3:
                raise RuntimeError("not yet")
            if body == "bad":
                done.set()

        stop = await c.subscribe(ch, "rt", h, retry_topic(SubscribeOptions(backoff=(0.3,), max_deliver=3)))
        await asyncio.sleep(1)
        for body in ("bad", "good"):
            await c.publish(ch, ["same"], Text(body), testing.TYPE_NAME)
        await asyncio.wait_for(done.wait(), 20)
        await stop()
        assert " ".join(seen) == "bad#1 good#1 bad#2 bad#3"
    finally:
        await b.close()


async def test_retry_in_process_blocks_partition() -> None:
    """The default mode keeps order: good waits for bad."""
    b = KafkaBackend(BROKERS, partitions=1)
    try:
        c = Core(b)
        ch = testing.channel()
        seen: list[str] = []
        done = asyncio.Event()

        async def h(segs: list[str], m: Message) -> None:
            body = m.data.decode()
            seen.append(f"{body}#{m.delivery}")
            if body == "bad" and m.delivery < 2:
                raise RuntimeError("not yet")
            if body == "good":
                done.set()

        stop = await c.subscribe(ch, "ip", h, retry_in_process(SubscribeOptions(backoff=(0.05,), max_deliver=3)))
        await asyncio.sleep(1)
        for body in ("bad", "good"):
            await c.publish(ch, ["same"], Text(body), testing.TYPE_NAME)
        await asyncio.wait_for(done.wait(), 20)
        await stop()
        assert " ".join(seen) == "bad#1 bad#2 good#1"
    finally:
        await b.close()


async def test_mapper() -> None:
    """The mapper names the topic, key, group and DLQ. A record key that
    is not the subject sends the subject in Gwag-Subject. A record
    without gwag headers on a channel whose pattern has no wildcards
    gets the pattern as its subject, and its key in Message.key."""
    ch = testing.channel()
    lit = Channel.new(ch.key + "Lit", "corp.orders")
    suffix = new_id()[:8]
    topic, lit_topic = f"map-{suffix}", f"corp-orders-{suffix}"
    b = KafkaBackend(
        BROKERS,
        topics=topic_table(
            {
                ch.key: Topic(name=topic, key=lambda subject, m: f"k-{subject}".encode(), dlq=topic + ".dead"),
                lit.key: Topic(name=lit_topic, group=lambda g: f"corp-{g}-{suffix}"),
            }
        ),
    )
    try:
        assert b.topic(ch) == topic
        assert b.default_topic(ch).name == ch.stream
        c = Core(b)
        got: asyncio.Queue[Message] = asyncio.Queue()

        async def h(segs: list[str], m: Message) -> None:
            await got.put(m)
            if m.key == b"fail":
                raise RuntimeError("dead")

        stops = [await c.subscribe(x, "m", h, SubscribeOptions(max_deliver=1)) for x in (ch, lit)]
        await c.publish(ch, ["a1"], Text("x"), testing.TYPE_NAME)
        m = await asyncio.wait_for(got.get(), 20)
        want = str(ch.pattern).replace("{id}", "a1")
        assert m.subject == want, m.subject
        assert m.key == f"k-{want}".encode()
        assert HEADER_SUBJECT not in m.headers

        # A record from a producer that is not gwag: no headers.
        p = AIOKafkaProducer(bootstrap_servers=BROKERS)
        await p.start()
        try:
            await p.send_and_wait(lit_topic, key=b"fail", value=b"corp")
        finally:
            await p.stop()
        m = await asyncio.wait_for(got.get(), 20)
        assert (m.subject, m.key, m.type) == ("corp.orders", b"fail", "")
        key, _, headers = (await _read_topic(dlq_subject(lit_topic), 1))[0]
        assert key == b"fail"
        assert headers.get("Gwag-Error") == "dead"
        assert headers.get(HEADER_SUBJECT) == "corp.orders"
        for s in stops:
            await s()
    finally:
        await b.close()
