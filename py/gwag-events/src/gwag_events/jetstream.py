"""NATS JetStream Backend (nats-py). Same behaviour as events/jetstream
in Go, so Go and Python consumers in one group share work.

One stream per channel, named stream_name(key), capturing the
channel's wildcard subject. A missing stream is created (file storage,
limits retention, max age 7d, duplicate window 2m, 1 replica) and an
existing one is never updated: the gateway owns its cluster policy. A
group is a durable consumer on that stream, used as it is when it
exists. The message id rides in Nats-Msg-Id, so republishing an id
inside the 2 minute window stores it once."""

from __future__ import annotations

import asyncio
import contextlib
import logging
from collections.abc import Callable

import nats.errors
import nats.js.api as api
import nats.js.errors
from nats.aio.client import Client as NATS
from nats.aio.msg import Msg as NatsMsg
from nats.js import JetStreamContext

from .channel import Channel, dlq_subject
from .types import (
    HEADER_ERROR,
    HEADER_NATS_MSG_ID,
    Handler,
    Message,
    Stop,
    SubscribeOptions,
    error_text,
)

_log = logging.getLogger("gwag_events")

STREAM_NAME_IN_USE = 10058
CONSUMER_EXISTS = 10148
_FETCH_TIMEOUT = 1.0


def stream_config(ch: Channel) -> api.StreamConfig:
    """The config a missing stream is created with."""
    return api.StreamConfig(
        name=ch.stream,
        subjects=[ch.pattern.wildcard()],
        storage=api.StorageType.FILE,
        retention=api.RetentionPolicy.LIMITS,
        max_age=7 * 24 * 3600.0,
        duplicate_window=120.0,
        num_replicas=1,
    )


class JetStreamBackend:
    """Backend over a connected nats-py client. close() stops the
    subscribers started here; it does not close the connection."""

    def __init__(
        self,
        nc: NATS,
        *,
        prefetch: int = 16,
        logf: Callable[[str], None] | None = None,
    ) -> None:
        self._nc = nc
        self._js: JetStreamContext = nc.jetstream()
        self._logf: Callable[[str], None] = logf or _log.error
        # How many messages a subscriber pulls ahead. Lower spreads a
        # group's work more evenly; prefetched messages count against ack_wait.
        self._prefetch = max(1, prefetch)
        self._ensured: set[str] = set()
        self._stops: set[Stop] = set()
        self._closed = False

    async def _ensure_stream(self, ch: Channel) -> None:
        if ch.stream in self._ensured:
            return
        try:
            try:
                await self._js.stream_info(ch.stream)
            except nats.js.errors.NotFoundError:
                try:
                    await self._js.add_stream(stream_config(ch))
                except nats.js.errors.APIError as e:
                    if e.err_code != STREAM_NAME_IN_USE:
                        raise
        except Exception as e:
            raise RuntimeError(f"events/jetstream: stream {ch.stream}: {e}") from e
        self._ensured.add(ch.stream)

    async def publish(self, channel: Channel, message: Message) -> None:
        await self._ensure_stream(channel)
        headers = dict(message.headers)
        if message.id:
            headers[HEADER_NATS_MSG_ID] = message.id
        try:
            await self._js.publish(message.subject, message.data, headers=headers)
        except Exception as e:
            raise RuntimeError(f"events/jetstream: publish {message.subject}: {e}") from e

    async def subscribe(self, channel: Channel, group: str, opts: SubscribeOptions, handler: Handler) -> Stop:
        if self._closed:
            raise RuntimeError("events/jetstream: closed")
        await self._ensure_stream(channel)
        await self._ensure_consumer(channel, group, opts)
        sub = await self._js.pull_subscribe_bind(group, stream=channel.stream)
        stopping = asyncio.Event()
        task = asyncio.get_running_loop().create_task(self._consume(channel, sub, opts, handler, stopping))
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
            with contextlib.suppress(Exception):
                await sub.unsubscribe()

        self._stops.add(stop)
        return stop

    async def _ensure_consumer(self, ch: Channel, group: str, opts: SubscribeOptions) -> None:
        try:
            await self._js.consumer_info(ch.stream, group)
            return
        except nats.js.errors.NotFoundError:
            pass
        except Exception as e:
            raise RuntimeError(f"events/jetstream: consumer {ch.stream}/{group}: {e}") from e
        cfg = api.ConsumerConfig(
            durable_name=group,
            ack_policy=api.AckPolicy.EXPLICIT,
            deliver_policy=api.DeliverPolicy.ALL if opts.deliver_all else api.DeliverPolicy.NEW,
            max_deliver=opts.max_deliver,
            ack_wait=opts.ack_wait,
        )
        try:
            await self._js.add_consumer(ch.stream, cfg)
        except nats.js.errors.APIError as e:
            if e.err_code != CONSUMER_EXISTS:
                raise RuntimeError(f"events/jetstream: create consumer {ch.stream}/{group}: {e}") from e
            await self._js.consumer_info(ch.stream, group)

    async def _consume(
        self,
        ch: Channel,
        sub: JetStreamContext.PullSubscription,
        opts: SubscribeOptions,
        handler: Handler,
        stopping: asyncio.Event,
    ) -> None:
        while not stopping.is_set():
            try:
                msgs = await sub.fetch(self._prefetch, timeout=_FETCH_TIMEOUT)
            except (asyncio.TimeoutError, nats.errors.TimeoutError):
                continue
            except (nats.errors.ConnectionClosedError, nats.errors.BadSubscriptionError):
                return
            except asyncio.CancelledError:
                raise
            except Exception as e:
                self._logf(f"events/jetstream: fetch {ch.stream}: {e}")
                with contextlib.suppress(asyncio.TimeoutError):
                    await asyncio.wait_for(stopping.wait(), 1.0)
                continue
            for i, msg in enumerate(msgs):
                if stopping.is_set():
                    # Give the rest back now instead of after ack_wait.
                    for rest in msgs[i:]:
                        with contextlib.suppress(Exception):
                            await rest.nak()
                    return
                await self._handle(ch, opts, handler, msg)

    async def _handle(self, ch: Channel, opts: SubscribeOptions, handler: Handler, msg: NatsMsg) -> None:
        headers = dict(msg.headers or {})
        m = Message(subject=msg.subject, data=msg.data, id=headers.pop(HEADER_NATS_MSG_ID, ""), headers=headers)
        with contextlib.suppress(Exception):
            m.delivery = msg.metadata.num_delivered
        try:
            await handler(m)
        except Exception as e:
            err = e
        else:
            await msg.ack()
            return
        if m.delivery >= opts.max_deliver:
            await self._dead_letter(ch, msg, err)
            await msg.term()
        else:
            await msg.nak(delay=opts.delay(m.delivery))

    async def _dead_letter(self, ch: Channel, msg: NatsMsg, cause: BaseException) -> None:
        headers = dict(msg.headers or {})
        headers[HEADER_ERROR] = error_text(cause)
        subject = dlq_subject(ch.stream)
        try:
            # A stream may capture gwag.dlq.>; without one, fall back to
            # a plain publish so live listeners still see it.
            try:
                await self._js.publish(subject, msg.data, headers=headers)
            except (nats.js.errors.NoStreamResponseError, nats.errors.NoRespondersError):
                await self._nc.publish(subject, msg.data, headers=headers)
        except Exception as e:
            self._logf(f"events/jetstream: dead-letter {msg.subject}: {e}")

    async def close(self) -> None:
        self._closed = True
        for stop in list(self._stops):
            await stop()
