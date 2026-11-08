"""A gwag_events Backend over Apache Kafka (aiokafka). Python half of
events/kafka in Go; the two share topics, groups, retry and dead-letter
topics and headers.

Default mapping (`topics=` changes every part of it):
  - one topic per channel, named channel.stream (EV_<pkg>_<Name>),
    created with 3 partitions and replication 1 if missing;
  - record key = subject (ordering per subject), value = the body the
    Serde wrote (plain proto by default), headers = Gwag-Type,
    Gwag-Channel, Gwag-Msg-Id and extras;
  - group = Kafka consumer group "<topic>.<group>";
  - a failed handler is retried in process with the backoff (the
    partition waits), then the record goes to topic gwag.dlq.<topic>
    with Gwag-Error and the offset is committed. retry_topic() moves
    retries to a per-group retry topic instead.

The producer is idempotent with acks=all.

No dedup: Kafka does not drop a record because its Gwag-Msg-Id was
seen. Handlers must be idempotent.
"""

from __future__ import annotations

import asyncio
import contextlib
import logging
import time
from collections.abc import Awaitable, Callable, Mapping
from dataclasses import dataclass, replace
from typing import Any, TypeAlias

from aiokafka import AIOKafkaConsumer, AIOKafkaProducer, ConsumerRebalanceListener
from aiokafka.admin import AIOKafkaAdminClient, NewTopic
from aiokafka.errors import TopicAlreadyExistsError
from aiokafka.structs import ConsumerRecord, TopicPartition
from gwag_events import (
    HEADER_CHANNEL,
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

__all__ = [
    "HEADER_ATTEMPT",
    "HEADER_RETRY_AT",
    "HEADER_SUBJECT",
    "KafkaBackend",
    "Mapper",
    "ResolvedTopic",
    "Topic",
    "retry_in_process",
    "retry_topic",
    "topic_table",
]

_log = logging.getLogger("gwag_events_kafka")

# Headers this backend adds.
HEADER_SUBJECT = "Gwag-Subject"  # the subject, when the record key is not the subject
HEADER_ATTEMPT = "Gwag-Attempt"  # delivery attempt of a record on a retry topic
HEADER_RETRY_AT = "Gwag-Retry-At"  # when a retry record is due, Unix milliseconds

Record: TypeAlias = ConsumerRecord[bytes, bytes]


@dataclass(frozen=True, slots=True)
class Topic:
    """Where one channel lives on Kafka. Unset fields take the
    defaults (see KafkaBackend.topic_of)."""

    # The topic. Default channel.stream.
    name: str = ""
    # Partitions for a topic the backend creates. Default the
    # backend's `partitions` (3). A group spreads over at most this
    # many subscribers.
    partitions: int = 0
    # Replication for a topic the backend creates. Default the
    # backend's `replication` (1); -1 is the broker default.
    replication: int = 0
    # The record key of a publish. Default: the subject. When the key
    # is not the subject, the subject rides in HEADER_SUBJECT.
    key: Callable[[str, Message], bytes] | None = None
    # The subject of a record without HEADER_SUBJECT or Gwag-Channel,
    # i.e. one a producer other than gwag wrote. Default: the channel
    # pattern when it has no wildcards, else the record key.
    subject: Callable[[Record], str] | None = None
    # The Kafka consumer group for a gwag group. Default "<name>.<group>".
    group: Callable[[str], str] | None = None
    # The dead-letter topic. Default "gwag.dlq.<name>".
    dlq: str = ""
    # The retry topic of a group (retry_topic() mode). Default
    # "gwag.retry.<name>.<group>". One per group, so a retry reaches
    # only the group that failed.
    retry: Callable[[str], str] | None = None


@dataclass(frozen=True, slots=True)
class ResolvedTopic:
    """A Topic with every default filled in."""

    name: str
    partitions: int
    replication: int
    key: Callable[[str, Message], bytes] | None
    subject: Callable[[Record], str]
    group: Callable[[str], str]
    dlq: str
    retry: Callable[[str], str]


# Returns the Topic of a channel. Leave a field unset to take its default.
Mapper = Callable[[Channel], Topic]


def topic_table(table: Mapping[str, Topic]) -> Mapper:
    """A Mapper over a table keyed by channel key
    ("orders.v1.OrderEvents"). Channels not in the table take the
    defaults."""
    return lambda ch: table.get(ch.key, Topic())


_RETRY_KEY = "kafka.retry"


def retry_in_process(opts: SubscribeOptions | None = None) -> SubscribeOptions:
    """opts that retry a failed record in the subscriber, waiting the
    backoff between attempts. The partition waits too: later records on
    it are not delivered until this one is acked or dead-lettered. The
    default."""
    o = opts or SubscribeOptions()
    return replace(o, backend={**o.backend, _RETRY_KEY: "process"})


def retry_topic(opts: SubscribeOptions | None = None) -> SubscribeOptions:
    """opts that move a failed record to the group's retry topic
    (Topic.retry) with HEADER_ATTEMPT and HEADER_RETRY_AT, and commit it
    on the channel topic, so the partition moves on. A second consumer
    reads the retry topic, waits until each record is due, and delivers
    it again; after max_deliver failed deliveries the record goes to the
    DLQ. Order per key is not kept across a retry. A record due later
    holds up the retry records behind it (one retry topic per group, not
    one per delay)."""
    o = opts or SubscribeOptions()
    return replace(o, backend={**o.backend, _RETRY_KEY: "topic"})


def _retry_mode(opts: SubscribeOptions) -> str:
    v = opts.backend.get(_RETRY_KEY, "process")
    if v not in ("process", "topic"):
        raise ValueError(f"kafka: unknown retry mode {v!r}")
    return str(v)


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
    """The Kafka Backend.

    brokers: "host:port,..." or a list.
    partitions, replication: defaults for topics the backend creates.
    topics: the channel-to-topic Mapper.
    client_config: aiokafka keyword arguments for every client the
      backend makes (producer, admin and each subscriber): security_protocol,
      ssl_context, sasl_mechanism, sasl_plain_username, sasl_plain_password,
      client_id, request_timeout_ms. They apply after the backend's own,
      so they win.
    producer_config, consumer_config: the same for the producer / the
      consumers only.
    """

    def __init__(
        self,
        brokers: str | list[str],
        *,
        partitions: int = 3,
        replication: int = 1,
        topics: Mapper | None = None,
        client_config: Mapping[str, Any] | None = None,
        producer_config: Mapping[str, Any] | None = None,
        consumer_config: Mapping[str, Any] | None = None,
        logf: Callable[[str], None] | None = None,
    ) -> None:
        self._brokers = brokers if isinstance(brokers, str) else ",".join(brokers)
        self._partitions = partitions
        self._replication = replication
        self._mapper = topics
        self._client_config = dict(client_config or {})
        self._producer_config = dict(producer_config or {})
        self._consumer_config = dict(consumer_config or {})
        self._logf: Callable[[str], None] = logf or _log.error
        self._producer: AIOKafkaProducer | None = None
        self._admin: AIOKafkaAdminClient | None = None
        self._lock = asyncio.Lock()
        self._topics: set[str] = set()
        self._stops: set[Stop] = set()
        self._closed = False

    # -- topic mapping --

    def topic_of(self, channel: Channel) -> ResolvedTopic:
        """channel's topic: the mapper's, defaults filled in."""
        t = self._mapper(channel) if self._mapper else Topic()
        return self._fill(channel, t)

    def default_topic(self, channel: Channel) -> ResolvedTopic:
        """channel's topic with every default."""
        return self._fill(channel, Topic())

    def topic(self, channel: Channel) -> str:
        """The topic name, which a Serde may derive a schema subject
        from (gwag_events.Topics)."""
        return self.topic_of(channel).name

    def _fill(self, channel: Channel, t: Topic) -> ResolvedTopic:
        name = t.name or channel.stream
        literal = not channel.pattern.segments
        pattern = str(channel.pattern)

        def default_subject(r: Record) -> str:
            return pattern if literal else (r.key or b"").decode()

        return ResolvedTopic(
            name=name,
            partitions=t.partitions or self._partitions,
            replication=t.replication or self._replication,
            key=t.key,
            subject=t.subject or default_subject,
            group=t.group or (lambda g: f"{name}.{g}"),
            dlq=t.dlq or dlq_subject(name),
            retry=t.retry or (lambda g: f"gwag.retry.{name}.{g}"),
        )

    # -- clients --

    async def _clients(self) -> tuple[AIOKafkaProducer, AIOKafkaAdminClient]:
        async with self._lock:
            if self._closed:
                raise RuntimeError("kafka: closed")
            if self._producer is None or self._admin is None:
                admin = AIOKafkaAdminClient(bootstrap_servers=self._brokers, **self._client_config)
                await admin.start()
                producer = AIOKafkaProducer(
                    bootstrap_servers=self._brokers,
                    **{"enable_idempotence": True, "acks": "all", **self._client_config, **self._producer_config},
                )
                await producer.start()
                self._admin, self._producer = admin, producer
            return self._producer, self._admin

    async def _ensure_topic(self, topic: str, partitions: int, replication: int) -> None:
        if topic in self._topics:
            return
        _, admin = await self._clients()
        try:
            await admin.create_topics([NewTopic(name=topic, num_partitions=partitions, replication_factor=replication)])
        except TopicAlreadyExistsError:
            pass
        except Exception as e:
            raise RuntimeError(f"kafka: create topic {topic}: {e}") from e
        self._topics.add(topic)

    async def _send(self, topic: str, key: bytes, value: bytes, headers: list[tuple[str, bytes]]) -> None:
        producer, _ = await self._clients()
        await producer.send_and_wait(topic, value=value, key=key, headers=headers)

    # -- Backend --

    async def publish(self, channel: Channel, message: Message) -> None:
        t = self.topic_of(channel)
        await self._ensure_topic(t.name, t.partitions, t.replication)
        key = message.subject.encode()
        if t.key is not None:
            key = t.key(message.subject, message)
            if key != message.subject.encode():
                message = _with_header(message, HEADER_SUBJECT, message.subject)
        await self._send(t.name, key, message.data, _headers(message))

    async def subscribe(self, channel: Channel, group: str, opts: SubscribeOptions, handler: Handler) -> Stop:
        mode = _retry_mode(opts)
        t = self.topic_of(channel)
        await self._ensure_topic(t.name, t.partitions, t.replication)
        # Create the DLQ topic now. A reader of the DLQ that starts
        # before the first dead letter would otherwise wait for a
        # metadata refresh to find a topic created after it.
        await self._ensure_topic(t.dlq, t.partitions, t.replication)
        sub = _Sub(self, channel, t, opts, handler, group)
        main_group = t.group(group)
        stops: list[Stop] = []
        try:
            stops.append(await sub.start(t.name, main_group, opts.deliver_all, sub.handle_main))
            if mode == "topic":
                sub.retry_topic = t.retry(group)
                await self._ensure_topic(sub.retry_topic, t.partitions, t.replication)
                stops.append(await sub.start(sub.retry_topic, main_group + ".retry", False, sub.handle_retry))
        except BaseException:
            for s in stops:
                await s()
            raise

        stopped = False

        async def stop() -> None:
            nonlocal stopped
            if stopped:
                return
            stopped = True
            self._stops.discard(stop)
            sub.stopping.set()
            for s in stops:
                await s()

        self._stops.add(stop)
        return stop

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

    def _consumer(self, topic: str, group: str, deliver_all: bool, listener: _Assigned) -> AIOKafkaConsumer:
        consumer = AIOKafkaConsumer(
            bootstrap_servers=self._brokers,
            group_id=group,
            **{
                "enable_auto_commit": False,
                "auto_offset_reset": "earliest" if deliver_all else "latest",
                **self._client_config,
                **self._consumer_config,
            },
        )
        consumer.subscribe([topic], listener=listener)
        return consumer


def _with_header(m: Message, k: str, v: str) -> Message:
    c = m.clone()
    c.headers[k] = v
    return c



class _Sub:
    def __init__(
        self,
        b: KafkaBackend,
        ch: Channel,
        t: ResolvedTopic,
        opts: SubscribeOptions,
        handler: Handler,
        group: str,
    ) -> None:
        self.b = b
        self.ch = ch
        self.t = t
        self.opts = opts
        self.h = handler
        self.group = group
        self.retry_topic = ""
        self.stopping = asyncio.Event()

    async def start(
        self,
        topic: str,
        group: str,
        deliver_all: bool,
        handle: Callable[[Record], Awaitable[None]],
    ) -> Stop:
        """Start a consumer that calls handle per record and wait for
        its partition assignment."""
        listener = _Assigned()
        consumer = self.b._consumer(topic, group, deliver_all, listener)
        await consumer.start()
        task = asyncio.get_running_loop().create_task(self._consume(consumer, topic, handle))

        async def stop() -> None:
            self.stopping.set()
            with contextlib.suppress(Exception):
                await task

        try:
            # Offsets of a new group resolve after assignment; resolve
            # them now so messages published after we return are seen.
            await asyncio.wait_for(self._assigned(consumer, listener), 30)
        except asyncio.TimeoutError:
            await stop()
            raise RuntimeError(f"kafka: group {group}: no partition assignment after 30s") from None
        return stop

    async def _assigned(self, consumer: AIOKafkaConsumer, listener: _Assigned) -> None:
        await listener.event.wait()
        for tp in consumer.assignment():
            await consumer.position(tp)

    async def _consume(self, consumer: AIOKafkaConsumer, topic: str, handle: Callable[[Record], Awaitable[None]]) -> None:
        try:
            while not self.stopping.is_set():
                batch = await consumer.getmany(timeout_ms=500)
                for records in batch.values():
                    for r in records:
                        if self.stopping.is_set():
                            return  # uncommitted: the group redelivers
                        await handle(r)
                if batch and not self.stopping.is_set():
                    with contextlib.suppress(Exception):
                        await consumer.commit()
        except asyncio.CancelledError:
            raise
        except Exception as e:
            self.b._logf(f"kafka: consume {topic}: {e}")
        finally:
            with contextlib.suppress(Exception):
                await consumer.stop()

    def message(self, r: Record) -> Message:
        """The message handlers see."""
        headers = {k: v.decode() for k, v in (r.headers or [])}
        m = Message(
            subject="",
            data=r.value or b"",
            id=headers.pop(HEADER_MSG_ID, ""),
            headers=headers,
            key=r.key or b"",
        )
        if HEADER_SUBJECT in headers:
            m.subject = headers.pop(HEADER_SUBJECT)
        elif headers.get(HEADER_CHANNEL):
            m.subject = (r.key or b"").decode()
        else:
            m.subject = self.t.subject(r)
        return m

    async def handle_main(self, r: Record) -> None:
        m = self.message(r)
        if self.retry_topic:
            m.delivery = 1
            try:
                await self.h(m)
            except Exception as e:
                await self.fail(r, m, e)
            return
        err: Exception | None = None
        for attempt in range(1, self.opts.max_deliver + 1):
            m.delivery = attempt
            try:
                await self.h(m)
                return
            except Exception as e:
                err = e
            if attempt < self.opts.max_deliver:
                with contextlib.suppress(asyncio.TimeoutError):
                    await asyncio.wait_for(self.stopping.wait(), self.opts.delay(attempt))
                if self.stopping.is_set():
                    return
        assert err is not None
        await self.dead_letter(r, m, err)

    async def handle_retry(self, r: Record) -> None:
        m = self.message(r)
        try:
            attempt = int(m.headers.get(HEADER_ATTEMPT, ""))
        except ValueError:
            attempt = 0
        attempt = max(attempt, 2)
        with contextlib.suppress(ValueError):
            wait = int(m.headers.get(HEADER_RETRY_AT, "")) / 1000 - time.time()
            if wait > 0:
                with contextlib.suppress(asyncio.TimeoutError):
                    await asyncio.wait_for(self.stopping.wait(), wait)
                if self.stopping.is_set():
                    return
        m.headers.pop(HEADER_ATTEMPT, None)
        m.headers.pop(HEADER_RETRY_AT, None)
        m.delivery = attempt
        try:
            await self.h(m)
        except Exception as e:
            await self.fail(r, m, e)

    async def fail(self, r: Record, m: Message, err: Exception) -> None:
        """Send a record that failed delivery m.delivery to the retry
        topic, or to the DLQ after the last attempt. Returns once the
        record is written, so the offset is committed only after that."""
        if m.delivery >= self.opts.max_deliver:
            await self.dead_letter(r, m, err)
            return
        nxt = _with_header(m, HEADER_ATTEMPT, str(m.delivery + 1))
        nxt.headers[HEADER_RETRY_AT] = str(int((time.time() + self.opts.delay(m.delivery)) * 1000))
        await self.produce(self.retry_topic, r, nxt)

    async def dead_letter(self, r: Record, m: Message, err: Exception) -> None:
        dead = _with_header(m, HEADER_ERROR, error_text(err))
        dead.headers.pop(HEADER_ATTEMPT, None)
        dead.headers.pop(HEADER_RETRY_AT, None)
        try:
            await self.b._ensure_topic(self.t.dlq, self.t.partitions, self.t.replication)
            await self.send(self.t.dlq, r, dead)
        except Exception as e:
            self.b._logf(f"kafka: dead-letter {m.subject}: {e}")

    async def send(self, topic: str, r: Record, m: Message) -> None:
        """Write r for topic with m's headers, keeping r's key and raw
        value (the Serde's bytes, not the decoded copy) and the
        subject."""
        raw = replace(m, data=r.value or b"")
        key = r.key or b""
        if key != m.subject.encode():
            raw = _with_header(raw, HEADER_SUBJECT, m.subject)
        await self.b._send(topic, key, raw.data, _headers(raw))

    async def produce(self, topic: str, r: Record, m: Message) -> None:
        """send, retrying until it is written or the subscriber stops:
        a retry record that is lost would drop the message."""
        while not self.stopping.is_set():
            try:
                await self.send(topic, r, m)
                return
            except Exception:
                with contextlib.suppress(asyncio.TimeoutError):
                    await asyncio.wait_for(self.stopping.wait(), 1)
