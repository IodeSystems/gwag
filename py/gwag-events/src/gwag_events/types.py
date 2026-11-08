"""Wire constants, Message, SubscribeOptions and the Backend protocol.
Mirrors events/events.go."""

from __future__ import annotations

import os
from collections.abc import Awaitable, Callable, Mapping
from dataclasses import dataclass, field, replace
from typing import Protocol

from .channel import Channel

HEADER_TYPE = "Gwag-Type"  # proto full name of the body message
HEADER_CHANNEL = "Gwag-Channel"  # channel key, "<proto package>.<name>"
HEADER_NATS_MSG_ID = "Nats-Msg-Id"  # message id on NATS (JetStream dedup)
HEADER_MSG_ID = "Gwag-Msg-Id"  # message id on other backends
HEADER_ERROR = "Gwag-Error"  # on a dead letter: the last handler error


@dataclass(slots=True)
class Message:
    """One message on the wire."""

    subject: str
    data: bytes = b""
    id: str = ""
    # HEADER_TYPE, HEADER_CHANNEL and any extra headers. Not the id
    # header: backends carry the id in `id`.
    headers: dict[str, str] = field(default_factory=dict)
    # 1-based delivery attempt. Set on receive.
    delivery: int = 0
    # The record key, on backends that have one (Kafka). Set on receive.
    key: bytes = b""

    def clone(self) -> Message:
        return replace(self, headers=dict(self.headers))

    @property
    def type(self) -> str:
        return self.headers.get(HEADER_TYPE, "")


@dataclass(frozen=True, slots=True)
class SubscribeOptions:
    """Subscribe configuration. Durations are seconds."""

    # Delivery attempts before dead-lettering.
    max_deliver: int = 5
    # Start a new group at the first stored message instead of at new ones.
    deliver_all: bool = False
    # Redelivery delay after failed attempt n is backoff[n-1]; the last
    # value repeats.
    backoff: tuple[float, ...] = (1.0, 5.0, 30.0)
    # How long a delivery may run before the backend redelivers it.
    ack_wait: float = 30.0
    # Backend-specific settings, keyed by backend (the Kafka package's
    # retry_topic() sets one). Other backends ignore them.
    backend: Mapping[str, object] = field(default_factory=dict)

    def resolved(self) -> SubscribeOptions:
        if self.max_deliver < 1:
            return replace(self, max_deliver=1)
        return self

    def delay(self, n: int) -> float:
        """Redelivery delay after failed delivery n (1-based)."""
        if not self.backoff:
            return 0.0
        n = min(max(n, 1), len(self.backoff))
        return self.backoff[n - 1]


@dataclass(frozen=True, slots=True)
class Subscription:
    """One (channel, group) a bus listens on. Pass these to the
    control plane as the service's subscriptions."""

    channel: str
    group: str


# Ends one subscriber; the group's position stays.
Stop = Callable[[], Awaitable[None]]
# Handles one delivery. Returning acks it; raising asks for redelivery.
Handler = Callable[[Message], Awaitable[None]]


class Backend(Protocol):
    """Moves messages. The backend owns delivery: ack when the handler
    returns, redeliver after `opts.delay(n)` when it raises, and after
    `opts.max_deliver` failed deliveries publish the message to
    `dlq_subject(channel.stream)` with the original headers plus
    HEADER_ERROR, then drop it."""

    async def publish(self, channel: Channel, message: Message) -> None:
        """Send message on message.subject, which matches channel.pattern."""
        ...

    async def subscribe(self, channel: Channel, group: str, opts: SubscribeOptions, handler: Handler) -> Stop:
        """Deliver messages of channel to handler. Subscribers with the
        same group share one queue; each group gets every message."""
        ...

    async def close(self) -> None:
        """Release the backend's resources."""
        ...


def new_id() -> str:
    """A random message id (32 hex digits, like the Go runtime)."""
    return os.urandom(16).hex()


def error_text(exc: BaseException) -> str:
    """What goes in HEADER_ERROR: the exception's message, or its
    class name when it has none."""
    return str(exc) or type(exc).__name__
