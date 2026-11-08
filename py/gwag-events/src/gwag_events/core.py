"""Core: what a generated bus wraps. Encodes, fills subjects, records
subscriptions and calls the Backend. Mirrors events/core.go."""

from __future__ import annotations

import contextvars
import logging
from collections.abc import Awaitable, Callable, Mapping, Sequence
from typing import Protocol

from .channel import Channel, valid_group
from .serde import ProtoSerde, Serde, Topics
from .types import (
    HEADER_CHANNEL,
    HEADER_TYPE,
    Backend,
    Message,
    Stop,
    SubscribeOptions,
    Subscription,
    new_id,
)

_log = logging.getLogger("gwag_events")

_current: contextvars.ContextVar[Message | None] = contextvars.ContextVar("gwag_events_message", default=None)


def message_from() -> Message | None:
    """The raw message being handled, inside a generated handler.
    None outside one."""
    return _current.get()


class ProtoMessage(Protocol):
    """What Core needs of a message: proto binary via bytes(msg).
    betterproto2 messages satisfy it."""

    def __bytes__(self) -> bytes: ...


# Gets the wildcard segment values (pattern order) and the message.
SegmentHandler = Callable[[list[str], Message], Awaitable[None]]


class Core:
    def __init__(
        self,
        backend: Backend,
        *,
        serde: Serde | None = None,
        logf: Callable[[str], None] | None = None,
    ) -> None:
        self._backend = backend
        self._serde: Serde = serde or ProtoSerde()
        self._logf: Callable[[str], None] = logf or _log.warning
        self._subs: list[Subscription] = []
        self._warned: set[tuple[str, str]] = set()

    @property
    def backend(self) -> Backend:
        return self._backend

    @property
    def serde(self) -> Serde:
        return self._serde

    def _topic(self, channel: Channel) -> str:
        if isinstance(self._backend, Topics):
            return self._backend.topic(channel)
        return channel.stream

    async def publish(
        self,
        channel: Channel,
        segments: Sequence[str],
        message: ProtoMessage,
        type_name: str,
        *,
        msg_id: str | None = None,
        headers: Mapping[str, str] | None = None,
    ) -> None:
        """Encode message and publish it on the subject built from
        segments (pattern order). type_name is the proto full name of
        the body, which goes in Gwag-Type. Extra headers cannot
        override Gwag-Type or Gwag-Channel."""
        try:
            subject = channel.pattern.subject(segments)
        except ValueError as e:
            raise ValueError(f"publish {channel.key}: {e}") from None
        h = dict(headers or {})
        h[HEADER_TYPE] = type_name
        h[HEADER_CHANNEL] = channel.key
        msg = Message(subject=subject, id=msg_id or new_id(), headers=h)
        try:
            await self._serde.encode(channel, self._topic(channel), bytes(message), msg)
        except Exception as e:
            e.add_note(f"publish {channel.key}: encode")
            raise
        await self._backend.publish(channel, msg)

    async def subscribe(
        self,
        channel: Channel,
        group: str,
        handler: SegmentHandler,
        options: SubscribeOptions | None = None,
    ) -> Stop:
        """Listen on channel as group. Messages whose subject does not
        match the pattern are skipped (acked)."""
        if not valid_group(group):
            raise ValueError(f"subscribe {channel.key}: group {group!r}: use letters, digits, '_' or '-'")
        opts = (options or SubscribeOptions()).resolved()

        plain = isinstance(self._serde, ProtoSerde)
        topic = self._topic(channel)

        async def deliver(raw: Message) -> None:
            m = raw
            if not plain:
                m = raw.clone()
                try:
                    await self._serde.decode(channel, topic, m)
                except Exception as e:
                    e.add_note(f"decode {channel.key}")
                    raise
            segs = channel.pattern.match(m.subject)
            if segs is None:
                self.skip(channel, m, f"subject {m.subject} does not match {channel.pattern}")
                return
            token = _current.set(m)
            try:
                await handler(segs, m)
            finally:
                _current.reset(token)

        stop = await self._backend.subscribe(channel, group, opts, deliver)
        sub = Subscription(channel.key, group)
        if sub not in self._subs:
            self._subs.append(sub)
        return stop

    def skip(self, channel: Channel, m: Message, why: str) -> None:
        """Log, once per (channel, message type), that a message was
        acked without a handler."""
        k = (channel.key, m.type)
        if k in self._warned:
            return
        self._warned.add(k)
        self._logf(f"events: {channel.key}: skipped message type {m.type!r}: {why} (logged once)")

    def subscriptions(self) -> list[Subscription]:
        """Every (channel, group) subscribe was called with, in call
        order. Stopping a subscriber does not remove it."""
        return list(self._subs)
