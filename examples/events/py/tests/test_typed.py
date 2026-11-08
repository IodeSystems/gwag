"""Tests of the generated Python API over the memory backend. Mirrors
examples/events/typed_test.go."""

import asyncio
from collections.abc import AsyncIterator, Callable

import pytest
from gwag_events import Channel, MemoryBackend, Message, SubscribeOptions, message_from

from proto_gen.orders.v1 import (
    AuditEntry,
    OrderCancelled,
    OrderCreated,
    OrderShipped,
    Payment,
    PaymentCaptured,
    PaymentRefunded,
)
from proto_gen.orders.v1.orders_events import (
    AuditSubject,
    OrderEventsEvent,
    OrderEventsSubject,
    OrdersBus,
    OrderShippedSubject,
    PaymentsSubject,
)


class Log:
    def __init__(self) -> None:
        self.got: list[str] = []

    def add(self, s: str) -> None:
        self.got.append(s)

    async def wait(self, n: int) -> list[str]:
        for _ in range(500):
            if len(self.got) >= n:
                break
            await asyncio.sleep(0.01)
        await asyncio.sleep(0.02)
        assert len(self.got) >= n, f"got {len(self.got)} of {n}: {self.got}"
        return list(self.got)


@pytest.fixture
async def mem() -> AsyncIterator[MemoryBackend]:
    b = MemoryBackend()
    yield b
    await b.close()


def bus_of(mem: MemoryBackend, logs: list[str] | None = None) -> OrdersBus:
    logf: Callable[[str], None] = (lambda s: None) if logs is None else logs.append
    return OrdersBus(mem, logf=logf)


async def test_plain_channel(mem: MemoryBackend) -> None:
    bus = bus_of(mem)
    log = Log()

    async def h(s: OrderShippedSubject, m: OrderShipped) -> None:
        raw = message_from()
        assert raw is not None
        log.add(f"{s.order_id} {m.carrier} {raw.subject} {raw.type}")

    await bus.on_order_shipped("notify", h)
    await bus.publish_order_shipped(OrderShippedSubject(order_id="o1"), OrderShipped(order_id="o1", carrier="ups"))
    assert (await log.wait(1)) == ["o1 ups events.orders.o1.shipped orders.v1.OrderShipped"]
    with pytest.raises(ValueError):
        await bus.publish_order_shipped(OrderShippedSubject(order_id="a.b"), OrderShipped())


async def test_oneof_channel(mem: MemoryBackend) -> None:
    bus = bus_of(mem)
    log = Log()

    async def captured(s: PaymentsSubject, m: PaymentCaptured) -> None:
        log.add(f"captured {s.region}/{s.p1} {m.order_id}")

    async def note(s: PaymentsSubject, text: str) -> None:
        log.add(f"note {text}")

    async def unknown(s: PaymentsSubject, m: Payment) -> None:
        log.add("unknown")

    # refunded has no handler: acked unhandled.
    await bus.on_payments("ledger", captured=captured, note=note, unknown=unknown)
    s = PaymentsSubject(region="eu", p1="card")
    await bus.publish_payments(s, Payment(captured=PaymentCaptured(order_id="o1")))
    await bus.publish_payments(s, Payment(refunded=PaymentRefunded(order_id="o1")))
    await bus.publish_payments(s, Payment(note="hi"))
    await bus.publish_payments(s, Payment())
    assert (await log.wait(3)) == ["captured eu/card o1", "note hi", "unknown"]


async def test_oneof_whole_message(mem: MemoryBackend) -> None:
    bus = bus_of(mem)
    log = Log()

    async def h(s: PaymentsSubject, m: Payment) -> None:
        log.add(f"{s.region} {m.note}")

    await bus.on_payments_message("all", h)
    await bus.publish_payments(PaymentsSubject(region="us", p1="gift"), Payment(note="x"))
    assert (await log.wait(1)) == ["us x"]


async def test_oneof_empty_string_case_is_set(mem: MemoryBackend) -> None:
    bus = bus_of(mem)
    log = Log()

    async def note(s: PaymentsSubject, text: str) -> None:
        log.add(f"note {text!r}")

    await bus.on_payments("ledger", note=note)
    await bus.publish_payments(PaymentsSubject(region="eu", p1="x"), Payment(note=""))
    assert (await log.wait(1)) == ["note ''"]


async def test_union_channel(mem: MemoryBackend) -> None:
    logged: list[str] = []
    bus = bus_of(mem, logged)
    log = Log()

    async def created(s: OrderEventsSubject, m: OrderCreated) -> None:
        log.add(f"created {s.order_id} {m.customer}")

    async def cancelled(s: OrderEventsSubject, m: OrderCancelled) -> None:
        log.add(f"cancelled {s.order_id} {m.reason}")

    await bus.on_order_events("billing", order_created=created, order_cancelled=cancelled)
    s = OrderEventsSubject(order_id="o7")
    await bus.publish_order_events(s, OrderCreated(order_id="o7", customer="ann"))
    await bus.publish_order_events(s, OrderCancelled(order_id="o7", reason="late"))
    # A type this code does not know: acked, logged once.
    ch = Channel.new("orders.v1.OrderEvents", "events.orders.{orderId}.lifecycle")
    for _ in range(2):
        await bus.core.publish(ch, ["o7"], AuditEntry(actor="x"), "orders.v1.AuditEntry")
    await bus.publish_order_events(s, OrderCreated(order_id="o7", customer="bob"))
    assert (await log.wait(3)) == ["created o7 ann", "cancelled o7 late", "created o7 bob"]
    assert len(logged) == 1, logged


async def test_union_publish_rejects_other_types(mem: MemoryBackend) -> None:
    bus = bus_of(mem)
    bad: OrderEventsEvent = OrderCreated()
    await bus.publish_order_events(OrderEventsSubject(order_id="o"), bad)
    with pytest.raises(TypeError):
        await bus.publish_order_events(OrderEventsSubject(order_id="o"), AuditEntry())  # type: ignore[arg-type]


async def test_union_unknown_handler(mem: MemoryBackend) -> None:
    bus = bus_of(mem)
    log = Log()

    async def unknown(s: OrderEventsSubject, m: Message) -> None:
        log.add(f"{m.type} {AuditEntry.parse(m.data).actor}")

    await bus.on_order_events("audit", unknown=unknown)
    ch = Channel.new("orders.v1.OrderEvents", "events.orders.{orderId}.lifecycle")
    await bus.core.publish(ch, ["o1"], AuditEntry(actor="eve"), "orders.v1.AuditEntry")
    assert (await log.wait(1)) == ["orders.v1.AuditEntry eve"]


async def test_rest_segment_and_subscriptions(mem: MemoryBackend) -> None:
    bus = bus_of(mem)
    log = Log()

    async def audit(s: AuditSubject, m: AuditEntry) -> None:
        log.add(f"{s.rest} {m.action}")

    async def shipped(s: OrderShippedSubject, m: OrderShipped) -> None:
        return None

    await bus.on_audit("archive", audit)
    await bus.on_order_shipped("notify", shipped)
    await bus.publish_audit(AuditSubject(rest="o1.refund"), AuditEntry(action="refund"))
    assert (await log.wait(1)) == ["o1.refund refund"]
    assert [(s.channel, s.group) for s in bus.subscriptions()] == [
        ("orders.v1.Audit", "archive"),
        ("orders.v1.OrderShipped", "notify"),
    ]


async def test_handler_error_dead_letters(mem: MemoryBackend) -> None:
    bus = bus_of(mem)
    calls: list[int] = []

    async def flaky(s: OrderShippedSubject, m: OrderShipped) -> None:
        calls.append(1)
        raise TimeoutError("deadline exceeded")

    await bus.on_order_shipped("flaky", flaky, options=SubscribeOptions(max_deliver=3, backoff=(0.001,)))
    await bus.publish_order_shipped(OrderShippedSubject(order_id="o1"), OrderShipped())
    for _ in range(500):
        if mem.dead_letters():
            break
        await asyncio.sleep(0.01)
    d = mem.dead_letters()[0]
    assert d.subject == "gwag.dlq.EV_orders_v1_OrderShipped"
    assert d.headers["Gwag-Error"] == "deadline exceeded"
    assert len(calls) == 3


async def test_bad_group(mem: MemoryBackend) -> None:
    bus = bus_of(mem)

    async def h(s: AuditSubject, m: AuditEntry) -> None:
        return None

    with pytest.raises(ValueError):
        await bus.on_audit("bad group", h)
