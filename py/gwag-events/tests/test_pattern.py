import pytest

from gwag_events import Channel, Pattern, Segment, SegmentKind, stream_name, valid_group


def test_parse_pattern() -> None:
    p = Pattern.parse("events.orders.{orderId}.*.>")
    assert p.segments == [
        Segment("orderId", SegmentKind.NAMED),
        Segment("P1", SegmentKind.POSITIONAL),
        Segment("Rest", SegmentKind.REST),
    ]
    assert p.wildcard() == "events.orders.*.*.>"
    s = p.subject(["o1", "eu", "a.b"])
    assert s == "events.orders.o1.eu.a.b"
    assert p.match(s) == ["o1", "eu", "a.b"]
    assert str(p) == "events.orders.{orderId}.*.>"


@pytest.mark.parametrize("p", ["", "a..b", "a.>.b", "a.{1x}", "a.{x}.{x}", "a.b*", "a.{x"])
def test_parse_pattern_errors(p: str) -> None:
    with pytest.raises(ValueError):
        Pattern.parse(p)


@pytest.mark.parametrize("v", ["", "b.c", "*", ">", "a b"])
def test_subject_rejects_bad_values(v: str) -> None:
    with pytest.raises(ValueError):
        Pattern.parse("a.{x}").subject([v])


def test_subject_rejects_wrong_count() -> None:
    with pytest.raises(ValueError):
        Pattern.parse("a.{x}").subject([])


@pytest.mark.parametrize(
    ("subject", "want"),
    [("a.b.c", True), ("a.b", False), ("a.b.c.d", False), ("x.b.c", False)],
)
def test_match(subject: str, want: bool) -> None:
    assert (Pattern.parse("a.*.c").match(subject) is not None) == want


def test_rest_needs_a_token() -> None:
    assert Pattern.parse("a.>").match("a") is None


def test_stream_name() -> None:
    assert stream_name("orders.v1.OrderEvents") == "EV_orders_v1_OrderEvents"
    ch = Channel.new("orders.v1.OrderEvents", "events.orders.{orderId}.lifecycle")
    assert ch.stream == "EV_orders_v1_OrderEvents"
    assert ch.pattern.wildcard() == "events.orders.*.lifecycle"


def test_valid_group() -> None:
    assert valid_group("billing-2_x")
    assert not valid_group("")
    assert not valid_group("a.b")
    assert not valid_group("a b")
