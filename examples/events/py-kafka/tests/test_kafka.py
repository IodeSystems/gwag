"""KafkaBackend against a real broker. $KAFKA_BROKERS names it;
../test.sh starts apache/kafka in docker and sets it."""

import asyncio
import os

import pytest
from aiokafka import AIOKafkaConsumer
from gwag_events import Backend, Message, dlq_subject, testing
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
    # Offsets of a new group resolve after assignment.
    await testing.CHECKS[name](Harness(new=_new, dead_letters=_dead_letters, settle=1.0, no_dedup=True))
