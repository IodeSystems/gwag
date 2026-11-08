"""Confluent's protobuf framing. Mirrors events/schemaregistry/wire.go
byte for byte."""

from __future__ import annotations

MAGIC_BYTE = 0


class NotFramedError(ValueError):
    """The data does not start with the magic byte."""


def _put_varint(out: bytearray, n: int) -> None:
    """Signed (zigzag) varint, as Go's binary.AppendVarint."""
    u = (n << 1) ^ (n >> 63) if n < 0 else n << 1
    while u >= 0x80:
        out.append((u & 0x7F) | 0x80)
        u >>= 7
    out.append(u)


def _get_varint(b: bytes, i: int) -> tuple[int, int]:
    u = shift = 0
    while True:
        if i >= len(b):
            raise ValueError("schema registry: truncated varint")
        c = b[i]
        i += 1
        u |= (c & 0x7F) << shift
        if c < 0x80:
            break
        shift += 7
        if shift > 63:
            raise ValueError("schema registry: varint too long")
    return (u >> 1) ^ -(u & 1), i


def frame(schema_id: int, indexes: list[int], body: bytes) -> bytes:
    """Confluent's protobuf framing of body: MAGIC_BYTE, the schema id
    (4 bytes, big endian), the message indexes, body.

    The indexes locate the message in its schema file: [i] is the i-th
    top-level message, [i, j] its j-th nested message. They are written
    as a zigzag varint count and zigzag varints; [0], the first
    top-level message, is written as a single 0."""
    out = bytearray([MAGIC_BYTE])
    out += schema_id.to_bytes(4, "big")
    if indexes == [0]:
        _put_varint(out, 0)
    else:
        _put_varint(out, len(indexes))
        for i in indexes:
            _put_varint(out, i)
    return bytes(out) + body


def unframe(data: bytes) -> tuple[int, list[int], bytes]:
    """Split framed data into schema id, message indexes and body."""
    if len(data) < 5 or data[0] != MAGIC_BYTE:
        raise NotFramedError("schema registry: data does not start with the magic byte")
    schema_id = int.from_bytes(data[1:5], "big")
    n, i = _get_varint(data, 5)
    if n == 0:
        return schema_id, [0], data[i:]
    if n < 0 or n > 1024:
        raise ValueError(f"schema registry: message index count {n}")
    indexes: list[int] = []
    for _ in range(n):
        v, i = _get_varint(data, i)
        if v < 0:
            raise ValueError("schema registry: bad message index")
        indexes.append(v)
    return schema_id, indexes, data[i:]
