"""Core with a Serde and a Topics backend, and the descriptor reader."""

import asyncio
import base64

from gwag_events import (
    Channel,
    Core,
    Files,
    MemoryBackend,
    Message,
    ProtoSerde,
    SubscribeOptions,
    default_files,
    register_file_descriptors,
)


class Upper:
    """A Serde that upper-cases the body and tags the message, and
    names the type when the record has none."""

    def __init__(self) -> None:
        self.topics: list[str] = []

    async def encode(self, channel: Channel, topic: str, body: bytes, out: Message) -> None:
        self.topics.append(topic)
        out.data = b"U:" + body.upper()
        out.headers["X-Serde"] = "upper"

    async def decode(self, channel: Channel, topic: str, m: Message) -> None:
        m.data = m.data.removeprefix(b"U:")
        m.headers.setdefault("Gwag-Type", "test.Text")


class Named(MemoryBackend):
    def topic(self, channel: Channel) -> str:
        return "custom-" + channel.stream


class Body:
    def __init__(self, text: str) -> None:
        self.text = text

    def __bytes__(self) -> bytes:
        return self.text.encode()


async def test_serde_round_trip() -> None:
    backend = Named()
    serde = Upper()
    core = Core(backend, serde=serde)
    ch = Channel.new("t.v1.Texts", "t.{id}")
    got: list[tuple[bytes, str, str]] = []
    done = asyncio.Event()

    async def handler(segs: list[str], m: Message) -> None:
        got.append((m.data, m.type, m.headers.get("X-Serde", "")))
        done.set()

    stop = await core.subscribe(ch, "g", handler, SubscribeOptions())
    await core.publish(ch, ["a"], Body("hello"), "test.Text")
    await asyncio.wait_for(done.wait(), 5)
    await stop()
    await backend.close()
    assert got == [(b"HELLO", "test.Text", "upper")]
    assert serde.topics == ["custom-" + ch.stream]


def test_default_serde_is_plain() -> None:
    assert isinstance(Core(MemoryBackend()).serde, ProtoSerde)


def _s(tag: int, v: bytes) -> bytes:
    return bytes([tag, len(v)]) + v


def _fdp() -> bytes:
    nested = _s(0x1A, _s(0x0A, b"Inner"))  # DescriptorProto.nested_type
    outer = _s(0x22, _s(0x0A, b"Outer") + nested)  # message_type
    other = _s(0x22, _s(0x0A, b"Other"))
    return _s(0x0A, b"a/b.proto") + _s(0x12, b"p.v1") + _s(0x1A, b"google/protobuf/any.proto") + outer + other


def test_descriptor_indexes() -> None:
    files = Files()
    info = files.add("a/b.proto", _fdp())
    assert info.package == "p.v1"
    assert info.dependency == ("google/protobuf/any.proto",)
    assert info.message_at([0]) == "p.v1.Outer"
    assert info.message_at([0, 0]) == "p.v1.Outer.Inner"
    assert info.message_at([1]) == "p.v1.Other"
    found = files.find_message("p.v1.Outer.Inner")
    assert found is not None
    assert found[1] == [0, 0]
    assert info.indexes_of("p.v1.Other") == [1]
    assert info.indexes_of("p.v1.Nope") is None


def test_register_file_descriptors() -> None:
    register_file_descriptors({"t/x.proto": base64.b64encode(_fdp()).decode()})
    assert default_files.find_message("p.v1.Other") is not None
