"""A gwag_events Backend over Apache Kafka (aiokafka). An example: copy
it into your project. It maps the same way as the Go one in
examples/events/go-kafka/kafka, so Go and Python services can share
topics and groups.

Mapping:
  - one topic per channel, named channel.stream (EV_<pkg>_<Name>),
    created with `partitions` partitions if missing;
  - record key = subject (ordering per subject), value = proto body,
    headers = Gwag-Type, Gwag-Channel, Gwag-Msg-Id and extras;
  - group = Kafka consumer group "<stream>.<group>";
  - a failed handler is retried in process with the backoff (the
    partition waits), then the record goes to topic gwag.dlq.<stream>
    with Gwag-Error and the offset is committed.

No dedup: Kafka does not drop a record because its Gwag-Msg-Id was
seen. Handlers must be idempotent.
"""

from __future__ import annotations

import asyncio
import contextlib
import logging
from collections.abc import Callable

from aiokafka import AIOKafkaConsumer, AIOKafkaProducer, ConsumerRebalanceListener
from aiokafka.admin import AIOKafkaAdminClient, NewTopic
from aiokafka.errors import TopicAlreadyExistsError
from aiokafka.structs import ConsumerRecord, TopicPartition
from gwag_events import (
    HEADER_ERROR,
    HEADER_MSG_ID,
    Channel,
    Handler,
    Message,
    Stop,
    SubscribeOptions,
)
from gwag_events.channel import dlq_subject
from gwag_events.types import error_text

_log = logging.getLogger("gwag_kafka")


def _headers(m: Message) -> list[tuple[str, bytes]]:
    out = [(k, v.encode()) for k, v in m.headers.items()]
    if m.id:
        out.append((HEADER_MSG_ID, m.id.encode()))
    return out


class _Assigned(ConsumerRebalanceListener):  # type: ignore[misc]
    def __init__(self) -> None:
        self.event = asyncio.Event()

    async def on_partitions_revoked(self, revoked: set[TopicPartition]) -> None:
        return None

    async def on_partitions_assigned(self, assigned: set[TopicPartition]) -> None:
        if assigned:
            self.event.set()


class KafkaBackend:
    def __init__(
        self,
        brokers: str | list[str],
        *,
        partitions: int = 3,
        logf: Callable[[str], None] | None = None,
    ) -> None:
        self._brokers = brokers if isinstance(brokers, str) else ",".join(brokers)
        self._partitions = partitions
        self._logf: Callable[[str], None] = logf or _log.error
        self._producer: AIOKafkaProducer | None = None
        self._admin: AIOKafkaAdminClient | None = None
        self._lock = asyncio.Lock()
        self._topics: set[str] = set()
        self._stops: set[Stop] = set()
        self._closed = False

    async def _clients(self) -> tuple[AIOKafkaProducer, AIOKafkaAdminClient]:
        async with self._lock:
            if self._closed:
                raise RuntimeError("kafka: closed")
            if self._producer is None or self._admin is None:
                admin = AIOKafkaAdminClient(bootstrap_servers=self._brokers)
                await admin.start()
                producer = AIOKafkaProducer(bootstrap_servers=self._brokers)
                await producer.start()
                self._admin, self._producer = admin, producer
            return self._producer, self._admin

    async def _ensure_topic(self, topic: str) -> None:
        if topic in self._topics:
            return
        _, admin = await self._clients()
        try:
            await admin.create_topics([NewTopic(name=topic, num_partitions=self._partitions, replication_factor=1)])
        except TopicAlreadyExistsError:
            pass
        except Exception as e:
            raise RuntimeError(f"kafka: create topic {topic}: {e}") from e
        self._topics.add(topic)

    async def _send(self, topic: str, m: Message) -> None:
        producer, _ = await self._clients()
        await producer.send_and_wait(topic, value=m.data, key=m.subject.encode(), headers=_headers(m))

    async def publish(self, channel: Channel, message: Message) -> None:
        await self._ensure_topic(channel.stream)
        await self._send(channel.stream, message)

    async def subscribe(self, channel: Channel, group: str, opts: SubscribeOptions, handler: Handler) -> Stop:
        await self._ensure_topic(channel.stream)
        # A reader of the dead-letter topic must find it before the first
        # dead letter, also on a broker that does not auto-create topics.
        await self._ensure_topic(dlq_subject(channel.stream))
        listener = _Assigned()
        consumer = AIOKafkaConsumer(
            bootstrap_servers=self._brokers,
            group_id=f"{channel.stream}.{group}",
            enable_auto_commit=False,
            auto_offset_reset="earliest" if opts.deliver_all else "latest",
        )
        consumer.subscribe([channel.stream], listener=listener)
        await consumer.start()
        stopping = asyncio.Event()
        task = asyncio.get_running_loop().create_task(self._consume(consumer, channel, opts, handler, stopping))
        stopped = False

        async def stop() -> None:
            nonlocal stopped
            if stopped:
                return
            stopped = True
            self._stops.discard(stop)
            stopping.set()
            with contextlib.suppress(Exception):
                await task

        try:
            # Offsets of a new group resolve after assignment; resolve
            # them now so messages published after we return are seen.
            await asyncio.wait_for(self._assigned(consumer, listener), 30)
        except asyncio.TimeoutError:
            await stop()
            raise RuntimeError(f"kafka: group {channel.stream}.{group}: no partition assignment after 30s") from None
        self._stops.add(stop)
        return stop

    async def _assigned(self, consumer: AIOKafkaConsumer, listener: _Assigned) -> None:
        await listener.event.wait()
        for tp in consumer.assignment():
            await consumer.position(tp)

    async def _consume(
        self,
        consumer: AIOKafkaConsumer,
        ch: Channel,
        opts: SubscribeOptions,
        handler: Handler,
        stopping: asyncio.Event,
    ) -> None:
        try:
            while not stopping.is_set():
                batch = await consumer.getmany(timeout_ms=500)
                for records in batch.values():
                    for r in records:
                        if stopping.is_set():
                            return  # uncommitted: the group redelivers
                        await self._handle(ch, opts, handler, r, stopping)
                if batch and not stopping.is_set():
                    with contextlib.suppress(Exception):
                        await consumer.commit()
        except asyncio.CancelledError:
            raise
        except Exception as e:
            self._logf(f"kafka: consume {ch.stream}: {e}")
        finally:
            with contextlib.suppress(Exception):
                await consumer.stop()

    async def _handle(
        self,
        ch: Channel,
        opts: SubscribeOptions,
        handler: Handler,
        r: ConsumerRecord[bytes, bytes],
        stopping: asyncio.Event,
    ) -> None:
        headers = {k: v.decode() for k, v in (r.headers or [])}
        m = Message(
            subject=(r.key or b"").decode(),
            data=r.value or b"",
            id=headers.pop(HEADER_MSG_ID, ""),
            headers=headers,
        )
        err: Exception | None = None
        for attempt in range(1, opts.max_deliver + 1):
            m.delivery = attempt
            try:
                await handler(m)
                return
            except Exception as e:
                err = e
            if attempt < opts.max_deliver:
                with contextlib.suppress(asyncio.TimeoutError):
                    await asyncio.wait_for(stopping.wait(), opts.delay(attempt))
                if stopping.is_set():
                    return
        assert err is not None
        dead = Message(
            subject=m.subject,
            data=m.data,
            id=m.id,
            headers={**{k: v for k, v in m.headers.items() if k != HEADER_ERROR}, HEADER_ERROR: error_text(err)},
        )
        dlq = dlq_subject(ch.stream)
        try:
            await self._ensure_topic(dlq)
            await self._send(dlq, dead)
        except Exception as e:
            self._logf(f"kafka: dead-letter {m.subject}: {e}")

    async def close(self) -> None:
        """Stop every subscriber and the producer."""
        for stop in list(self._stops):
            await stop()
        async with self._lock:
            if self._closed:
                return
            self._closed = True
            if self._producer is not None:
                await self._producer.stop()
            if self._admin is not None:
                await self._admin.close()
