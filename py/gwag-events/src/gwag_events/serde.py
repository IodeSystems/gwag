"""Serde: the wire format of a message body. Mirrors events/serde.go.

Core calls the serde on every publish and every delivery, so any
backend carries any wire format. The default, ProtoSerde, writes plain
proto binary; the Gwag-* headers are set by Core either way."""

from __future__ import annotations

from typing import Protocol, runtime_checkable

from .channel import Channel
from .types import Message


class Serde(Protocol):
    async def encode(self, channel: Channel, topic: str, body: bytes, out: Message) -> None:
        """Set out.data for body (the message's proto binary). out
        already holds subject, id and the Gwag-Type / Gwag-Channel
        headers; encode may add headers. topic is the backend's name for
        the channel (Topics.topic, else channel.stream)."""
        ...

    async def decode(self, channel: Channel, topic: str, m: Message) -> None:
        """Rewrite m.data to plain proto binary of the body message.
        When m has no Gwag-Type header (a record from a producer that
        is not gwag) decode sets it if the wire format names the type.
        m is a copy; the backend keeps the original for redelivery and
        dead letters."""
        ...


class ProtoSerde:
    """The default Serde: the body is proto binary, nothing else."""

    async def encode(self, channel: Channel, topic: str, body: bytes, out: Message) -> None:
        out.data = body

    async def decode(self, channel: Channel, topic: str, m: Message) -> None:
        return None


@runtime_checkable
class Topics(Protocol):
    """Implemented by a backend whose name for a channel is not
    channel.stream (the Kafka backend's topic mapper). Core passes the
    name to the Serde, which may derive a schema subject from it."""

    def topic(self, channel: Channel) -> str: ...
