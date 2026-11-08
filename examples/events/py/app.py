"""The sample service: listens on every channel of orders/v1/orders.proto
and publishes one message of each shape. Shared by main.py and
interop.py. The output lines match examples/events/go-jetstream."""

import asyncio
from collections.abc import Callable

from gwag_events import Stop

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
    OrderEventsSubject,
    OrdersBus,
    OrderShippedSubject,
    PaymentsSubject,
)

Emit = Callable[[str], None]


async def listen_all(bus: OrdersBus, group: str, emit: Emit, *, sleep: float = 0.0) -> list[Stop]:
    """Listen on all four channels as `group`. Each delivery calls
    emit(line). Returns the stop functions."""

    async def pause() -> None:
        if sleep:
            await asyncio.sleep(sleep)

    async def created(s: OrderEventsSubject, m: OrderCreated) -> None:
        await pause()
        emit(f"OrderEvents   {s.order_id} created by {m.customer}")

    async def cancelled(s: OrderEventsSubject, m: OrderCancelled) -> None:
        await pause()
        emit(f"OrderEvents   {s.order_id} cancelled: {m.reason}")

    async def captured(s: PaymentsSubject, m: PaymentCaptured) -> None:
        await pause()
        emit(f"Payments      {s.region}/{s.p1} captured {m.amount_cents} for {m.order_id}")

    async def refunded(s: PaymentsSubject, m: PaymentRefunded) -> None:
        await pause()
        emit(f"Payments      {s.region}/{s.p1} refunded {m.amount_cents}: {m.reason}")

    async def note(s: PaymentsSubject, text: str) -> None:
        await pause()
        emit(f"Payments      {s.region}/{s.p1} note {text}")

    async def shipped(s: OrderShippedSubject, m: OrderShipped) -> None:
        await pause()
        emit(f"OrderShipped  {s.order_id} via {m.carrier}")

    async def audit(s: AuditSubject, m: AuditEntry) -> None:
        await pause()
        emit(f"Audit         {s.rest}: {m.actor} {m.action}")

    # Each on_* call is one durable consumer per (channel, group).
    # Replicas of this service calling it with the same group share
    # the messages.
    return [
        await bus.on_order_events(group, order_created=created, order_cancelled=cancelled),
        await bus.on_payments(group, captured=captured, refunded=refunded, note=note),
        await bus.on_order_shipped(group, shipped),
        await bus.on_audit(group, audit),
    ]


async def publish_all(bus: OrdersBus) -> None:
    """One message per channel shape, plus a second order event and a
    payment of each other kind: seven in all."""
    await bus.publish_order_events(
        OrderEventsSubject(order_id="o-1001"), OrderCreated(order_id="o-1001", customer="ann")
    )
    await bus.publish_payments(
        PaymentsSubject(region="eu", p1="card"),
        Payment(captured=PaymentCaptured(order_id="o-1001", amount_cents=4200)),
    )
    await bus.publish_order_shipped(
        OrderShippedSubject(order_id="o-1001"), OrderShipped(order_id="o-1001", carrier="ups")
    )
    await bus.publish_order_events(
        OrderEventsSubject(order_id="o-1002"), OrderCancelled(order_id="o-1002", reason="out of stock")
    )
    await bus.publish_payments(
        PaymentsSubject(region="eu", p1="card"),
        Payment(refunded=PaymentRefunded(order_id="o-1002", amount_cents=1500, reason="cancelled")),
    )
    await bus.publish_audit(AuditSubject(rest="o-1002.refund"), AuditEntry(actor="support", action="refund"))
    await bus.publish_payments(PaymentsSubject(region="us", p1="gift"), Payment(note="thank you"))
