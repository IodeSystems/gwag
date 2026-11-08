"""Proto file descriptors, as far as the wire formats need them: which
file declares a message, what it imports, and where the message sits in
the file (Confluent's message indexes).

Generated code registers the serialized FileDescriptorProto of its
file and of each file that one imports (`gwag gen --lang python`);
betterproto2 messages carry no descriptor of their own. The bytes are
what the Go runtime registers with a Schema Registry, so the two
produce the same schema. This module reads just the fields it needs
from them (name, package, dependency, message_type, nested_type) and
has no protobuf dependency."""

from __future__ import annotations

import base64
from collections.abc import Mapping
from dataclasses import dataclass, field


@dataclass(frozen=True, slots=True)
class MessageInfo:
    name: str
    nested: tuple[MessageInfo, ...] = ()


@dataclass(frozen=True, slots=True)
class FileInfo:
    """What parse_file reads of a FileDescriptorProto."""

    name: str
    package: str
    dependency: tuple[str, ...]
    messages: tuple[MessageInfo, ...]
    raw: bytes = field(repr=False, default=b"")

    def message_at(self, indexes: list[int]) -> str:
        """The full name of the message at indexes: [i] is the i-th
        top-level message, [i, j] its j-th nested message."""
        if not indexes:
            raise ValueError("no message indexes")
        ms = self.messages
        parts: list[str] = []
        for i in indexes:
            if i < 0 or i >= len(ms):
                raise ValueError(f"message index {indexes} not in {self.name}")
            parts.append(ms[i].name)
            ms = ms[i].nested
        return ".".join(([self.package] if self.package else []) + parts)

    def indexes_of(self, full_name: str) -> list[int] | None:
        """The indexes of the message named full_name (top-level or
        nested), or None."""
        prefix = self.package + "." if self.package else ""
        if not full_name.startswith(prefix):
            return None
        want = full_name[len(prefix) :].split(".")
        ms = self.messages
        out: list[int] = []
        for part in want:
            for i, m in enumerate(ms):
                if m.name == part:
                    out.append(i)
                    ms = m.nested
                    break
            else:
                return None
        return out


def _varint(b: bytes, i: int) -> tuple[int, int]:
    n = shift = 0
    while True:
        if i >= len(b):
            raise ValueError("truncated varint")
        c = b[i]
        i += 1
        n |= (c & 0x7F) << shift
        if c < 0x80:
            return n, i
        shift += 7
        if shift > 63:
            raise ValueError("varint too long")


def _fields(b: bytes) -> list[tuple[int, bytes]]:
    """The length-delimited fields of b as (number, bytes). Other wire
    types are skipped."""
    out: list[tuple[int, bytes]] = []
    i = 0
    while i < len(b):
        tag, i = _varint(b, i)
        num, wt = tag >> 3, tag & 7
        if wt == 0:
            _, i = _varint(b, i)
        elif wt == 1:
            i += 8
        elif wt == 5:
            i += 4
        elif wt == 2:
            n, i = _varint(b, i)
            if i + n > len(b):
                raise ValueError("truncated field")
            out.append((num, b[i : i + n]))
            i += n
        else:
            raise ValueError(f"unsupported wire type {wt}")
    return out


def _message(b: bytes) -> MessageInfo:
    name = ""
    nested: list[MessageInfo] = []
    for num, v in _fields(b):
        if num == 1:
            name = v.decode()
        elif num == 3:
            nested.append(_message(v))
    return MessageInfo(name, tuple(nested))


def parse_file(raw: bytes) -> FileInfo:
    """Read a serialized FileDescriptorProto."""
    name = package = ""
    deps: list[str] = []
    msgs: list[MessageInfo] = []
    for num, v in _fields(raw):
        if num == 1:
            name = v.decode()
        elif num == 2:
            package = v.decode()
        elif num == 3:
            deps.append(v.decode())
        elif num == 4:
            msgs.append(_message(v))
    return FileInfo(name, package, tuple(deps), tuple(msgs), raw)


class Files:
    """A set of files by path, with a lookup of message by full name."""

    def __init__(self) -> None:
        self._files: dict[str, FileInfo] = {}
        self._by_message: dict[str, tuple[str, list[int]]] = {}

    def add(self, path: str, raw: bytes) -> FileInfo:
        info = parse_file(raw)
        self._files[path] = info
        self._index(path, info.package, info.messages, [])
        return info

    def _index(self, path: str, prefix: str, ms: tuple[MessageInfo, ...], idx: list[int]) -> None:
        for i, m in enumerate(ms):
            full = f"{prefix}.{m.name}" if prefix else m.name
            self._by_message[full] = (path, [*idx, i])
            self._index(path, full, m.nested, [*idx, i])

    def file(self, path: str) -> FileInfo | None:
        return self._files.get(path)

    def find_message(self, full_name: str) -> tuple[FileInfo, list[int]] | None:
        """The registered file declaring full_name and its message
        indexes, or None."""
        hit = self._by_message.get(full_name)
        if hit is None:
            return None
        return self._files[hit[0]], list(hit[1])


# What generated code registers into.
default_files = Files()


def register_file_descriptors(files: Mapping[str, str]) -> None:
    """Register serialized FileDescriptorProtos (base64) by import
    path. Called by generated code."""
    for path, b64 in files.items():
        default_files.add(path, base64.b64decode(b64))
