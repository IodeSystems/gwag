"""The Serde against an in-memory registry."""

import base64

import pytest
from gwag_events import HEADER_CHANNEL, HEADER_TYPE, Channel, Files, Message

from gwag_events_schemaregistry import (
    HEADER_SCHEMA_ID,
    Client,
    Config,
    NotRegisteredError,
    Reference,
    Schema,
    Serde,
    SubjectNameStrategy,
    frame,
    record_name_strategy,
    strategy_by_name,
    topic_name_strategy,
    topic_record_name_strategy,
)
from gwag_events_schemaregistry.srtest import FakeRegistry

CH = Channel.new("shop.v1.Orders", "shop.orders")


def _s(tag: int, v: bytes) -> bytes:
    return bytes([tag, len(v)]) + v


def _msg(name: str, *nested: str) -> bytes:
    return _s(0x22, _s(0x0A, name.encode()) + b"".join(_s(0x1A, _s(0x0A, n.encode())) for n in nested))


def _file(name: str, package: str, deps: list[str], *msgs: bytes) -> bytes:
    return _s(0x0A, name.encode()) + _s(0x12, package.encode()) + b"".join(_s(0x1A, d.encode()) for d in deps) + b"".join(msgs)


MONEY = _file("shop/money.proto", "shop.v1", ["google/protobuf/any.proto"], _msg("Money"))
ORDER = _file("shop/order.proto", "shop.v1", ["shop/money.proto", "google/protobuf/any.proto"], _msg("Cart"), _msg("Order", "Line"))


def _files() -> Files:
    f = Files()
    f.add("shop/money.proto", MONEY)
    f.add("shop/order.proto", ORDER)
    return f


def _b64(b: bytes) -> str:
    return base64.b64encode(b).decode()


def _setup(
    *,
    auto_register: bool = False,
    use_latest: bool = False,
    subject_name_strategy: SubjectNameStrategy = topic_name_strategy,
    type_names: dict[str, str] | None = None,
) -> tuple[FakeRegistry, Client, Serde]:
    reg = FakeRegistry()
    client = Client(Config(url="http://sr.test", http_client=reg.http_client()))
    serde = Serde(
        client,
        files=_files(),
        auto_register=auto_register,
        use_latest=use_latest,
        subject_name_strategy=subject_name_strategy,
        type_names=type_names,
    )
    return reg, client, serde


def _out(name: str) -> Message:
    return Message(subject="shop.orders", headers={HEADER_TYPE: name, HEADER_CHANNEL: CH.key})


async def test_lookup_only_fails_when_not_registered() -> None:
    _, _, serde = _setup()
    with pytest.raises(NotRegisteredError):
        await serde.encode(CH, "orders", b"x", _out("shop.v1.Order"))


async def test_auto_register_frames_and_caches() -> None:
    reg, client, serde = _setup(auto_register=True)
    m = _out("shop.v1.Order")
    await serde.encode(CH, "orders", b"body", m)
    # Money is registered first, as a reference under its import path.
    money_id, _ = reg.add("shop/money.proto", Schema(_b64(MONEY)))
    order = await client.subject_version("orders-value", "1")
    assert order.references == [Reference("shop/money.proto", "shop/money.proto", 1)]
    assert order.schema == _b64(ORDER)
    # Order is the second message of the file: indexes [1].
    assert m.data == frame(order.id, [1], b"body")
    assert money_id == 1
    n = sum(reg.requests.values())
    await serde.encode(CH, "orders", b"again", _out("shop.v1.Order"))
    assert sum(reg.requests.values()) == n  # cached


async def test_nested_message_indexes() -> None:
    _, _, serde = _setup(auto_register=True)
    m = _out("shop.v1.Order.Line")
    await serde.encode(CH, "orders", b"", m)
    assert m.data[5:7] == bytes([4, 2]) and m.data[7] == 0  # count 2, [1, 0]


async def test_lookup_after_registration() -> None:
    reg, client, serde = _setup(auto_register=True)
    await serde.encode(CH, "orders", b"x", _out("shop.v1.Order"))
    other = Serde(client, files=_files())
    m = _out("shop.v1.Cart")
    await other.encode(CH, "orders", b"y", m)
    assert m.data[5] == 0  # [0]
    assert m.data[1:5] == (await client.subject_version("orders-value")).id.to_bytes(4, "big")


async def test_strategies() -> None:
    reg, _, serde = _setup(auto_register=True, subject_name_strategy=record_name_strategy)
    await serde.encode(CH, "orders", b"", _out("shop.v1.Order"))
    assert "POST /subjects/shop.v1.Order/versions" in reg.requests
    assert topic_record_name_strategy("t", "a.B") == "t-a.B"
    with pytest.raises(ValueError):
        strategy_by_name("nope")


async def test_use_schema_id_and_latest() -> None:
    reg, client, _ = _setup()
    reg.add("shop/money.proto", Schema(_b64(MONEY)))
    sid, _ = reg.add("orders-value", Schema(_b64(ORDER), references=[Reference("shop/money.proto", "shop/money.proto", 1)]))
    for serde in (Serde(client, files=Files(), use_schema_id=sid), Serde(client, files=Files(), use_latest=True)):
        m = _out("shop.v1.Order")
        await serde.encode(CH, "orders", b"z", m)
        assert m.data == frame(sid, [1], b"z")
    serde = Serde(client, files=Files(), use_schema_id=sid)
    with pytest.raises(LookupError):
        await serde.encode(CH, "orders", b"", _out("shop.v1.Nope"))


def test_options_are_exclusive() -> None:
    msg = "schema registry: auto-register, use-latest and use-schema-id are mutually exclusive; set at most one"
    client = Client(Config(url="http://sr.test", http_client=FakeRegistry().http_client()))
    for kw in (
        {"auto_register": True, "use_latest": True},
        {"use_schema_id": 3, "auto_register": True},
        {"use_latest": True, "use_schema_id": 3},
    ):
        with pytest.raises(ValueError) as e:
            Serde(client, files=_files(), **kw)
        assert str(e.value) == msg
    Serde(client, files=_files(), use_latest=True)


async def test_decode_without_gwag_headers() -> None:
    reg, client, serde = _setup()
    sid, _ = reg.add("orders-value", Schema(_b64(ORDER)))
    m = Message(subject="shop.orders", data=frame(sid, [1, 0], b"body"))
    await serde.decode(CH, "orders", m)
    assert m.data == b"body"
    assert m.type == "shop.v1.Order.Line"
    assert m.headers[HEADER_SCHEMA_ID] == str(sid)


async def test_decode_plain_and_header_checks() -> None:
    reg, _, serde = _setup(type_names={"shop.v1.Order": "mine.v1.OrderShadow"})
    sid, _ = reg.add("orders-value", Schema(_b64(ORDER)))
    plain = Message(subject="s", data=b"\x0a\x01a", headers={HEADER_TYPE: "x.Y"})
    await serde.decode(CH, "orders", plain)
    assert plain.data == b"\x0a\x01a" and plain.type == "x.Y"
    shadow = Message(subject="s", data=frame(sid, [1], b"b"))
    await serde.decode(CH, "orders", shadow)
    assert shadow.type == "mine.v1.OrderShadow"
    for ok in ("mine.v1.OrderShadow", "shop.v1.Order"):
        await serde.decode(CH, "orders", Message(subject="s", data=frame(sid, [1], b"b"), headers={HEADER_TYPE: ok}))
    with pytest.raises(ValueError, match="frames shop.v1.Order"):
        await serde.decode(CH, "orders", Message(subject="s", data=frame(sid, [1], b"b"), headers={HEADER_TYPE: "other.T"}))
    with pytest.raises(ValueError, match="not in"):
        await serde.decode(CH, "orders", Message(subject="s", data=frame(sid, [9], b"b")))


async def test_unknown_schema_id_fails() -> None:
    _, _, serde = _setup()
    with pytest.raises(Exception, match="404"):
        await serde.decode(CH, "orders", Message(subject="s", data=frame(99, [0], b"")))
