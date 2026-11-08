"""KafkaBackend against a real broker. $KAFKA_BROKERS names it;
../test.sh starts apache/kafka in docker and sets it."""

import asyncio
import os
import uuid

import pytest
from aiokafka import AIOKafkaConsumer
from gwag_events import Backend, Channel, Message, SubscribeOptions, dlq_subject, testing
from gwag_events.testing import Harness

from gwag_kafka import KafkaBackend

BROKERS = os.environ.get("KAFKA_BROKERS", "")
pytestmark = pytest.mark.skipif(not BROKERS, reason="KAFKA_BROKERS not set; run ../test.sh")


async def _new() -> Backend:
    return KafkaBackend(BROKERS)


async def _dead_letters(b: Backend, stream: str, n: int) -> list[Message]:
    topic = dlq_subject(stream)
    consumer = AIOKafkaConsumer(topic, bootstrap_servers=BROKERS, auto_offset_reset="earliest")
    await consumer.start()
    out: list[Message] = []
    try:
        async with asyncio.timeout(30):
            while len(out) < n:
                for records in (await consumer.getmany(timeout_ms=500)).values():
                    for r in records:
                        out.append(
                            Message(
                                subject=r.topic,
                                data=r.value or b"",
                                headers={k: v.decode() for k, v in (r.headers or [])},
                            )
                        )
    except TimeoutError:
        raise AssertionError(f"got {len(out)} dead letters, want {n}") from None
    finally:
        await consumer.stop()
    return out


# Kafka does not drop a record because its Gwag-Msg-Id was seen, so the
# dedup check does not apply.
@pytest.mark.parametrize("name", [n for n in testing.CHECKS if n != "dedup"])
async def test_conformance(name: str) -> None:
    # settle: a second member joining a group triggers a rebalance.
    await testing.CHECKS[name](Harness(new=_new, dead_letters=_dead_letters, settle=1.0, no_dedup=True))


async def test_publish_right_after_subscribe() -> None:
    """aiokafka resolves a new group's start before subscribe returns."""
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
