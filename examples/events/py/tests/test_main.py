"""main.py end to end over JetStream, like examples/events/go-jetstream."""

from .conftest import Proc, needs_go

pytestmark = needs_go


def test_main(nats_url: str) -> None:
    out = Proc(["uv", "run", "--locked", "python", "main.py"], nats_url).run_to_exit()
    for want in [
        "subscribes    orders.v1.OrderEvents as example-",
        "OrderEvents   o-1001 created by ann",
        "OrderEvents   o-1002 cancelled: out of stock",
        "Payments      eu/card captured 4200 for o-1001",
        "Payments      eu/card refunded 1500: cancelled",
        "Payments      us/gift note thank you",
        "OrderShipped  o-1001 via ups",
        "Audit         o-1002.refund: support refund",
    ]:
        assert any(line.startswith(want) for line in out), f"missing {want!r} in {out}"
