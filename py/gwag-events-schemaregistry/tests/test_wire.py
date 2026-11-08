"""Framing vectors; the same as events/schemaregistry TestFrame in Go."""

import pytest

from gwag_events_schemaregistry import NotFramedError, frame, unframe


@pytest.mark.parametrize(
    ("idx", "want"),
    [
        ([0], bytes([0, 0, 0, 1, 2, 0]) + b"b"),
        ([1], bytes([0, 0, 0, 1, 2, 2, 2]) + b"b"),
        ([2, 0], bytes([0, 0, 0, 1, 2, 4, 4, 0]) + b"b"),
    ],
)
def test_frame(idx: list[int], want: bytes) -> None:
    got = frame(258, idx, b"b")
    assert got == want
    assert unframe(got) == (258, idx, b"b")


def test_unframe_plain() -> None:
    with pytest.raises(NotFramedError):
        unframe(bytes([1, 2, 3, 4, 5, 6]))


def test_large_indexes_round_trip() -> None:
    idx = [300, 0, 70000]
    assert unframe(frame(1 << 31, idx, b"x")) == (1 << 31, idx, b"x")
