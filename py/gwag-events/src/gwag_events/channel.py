"""Channels and subject patterns. Mirrors events/pattern.go and the
channel half of events/events.go; the two must agree on every input."""

from __future__ import annotations

import enum
import re
from collections.abc import Sequence
from dataclasses import dataclass

DLQ_PREFIX = "gwag.dlq."


class SegmentKind(enum.Enum):
    NAMED = "named"  # "{name}": one token, named
    POSITIONAL = "positional"  # "*": one token, named P1, P2, ...
    REST = "rest"  # trailing ">": one or more tokens, named Rest


@dataclass(frozen=True, slots=True)
class Segment:
    """One wildcard segment of a pattern. `name` is as in the Go
    runtime ("orderId", "P1", "Rest"); the generator cases it."""

    name: str
    kind: SegmentKind


_IDENT = re.compile(r"[A-Za-z_][A-Za-z0-9_]*\Z")
_BAD_TOKEN_CHARS = set("*>{} \t\r\n")
_BAD_SUBJECT_CHARS = set(".*> \t\r\n")


def _valid_token(t: str) -> bool:
    return t != "" and not (set(t) & _BAD_SUBJECT_CHARS)


class Pattern:
    """A parsed channel pattern. NATS subject grammar: "*" matches one
    token, ">" (last token only) matches the rest. "{name}" matches
    like "*" and names the token."""

    __slots__ = ("_raw", "_tokens", "_segs")

    def __init__(self, raw: str, tokens: list[str], segs: list[Segment]) -> None:
        self._raw = raw
        self._tokens = tokens  # literal token, or "" for a wildcard
        self._segs = segs

    @classmethod
    def parse(cls, p: str) -> Pattern:
        if p == "":
            raise ValueError("pattern is empty")
        parts = p.split(".")
        tokens: list[str] = []
        segs: list[Segment] = []
        seen: set[str] = set()
        positional = 0
        for i, t in enumerate(parts):
            if t == "":
                raise ValueError(f"pattern {p!r}: empty token")
            elif t == "*":
                positional += 1
                segs.append(Segment(f"P{positional}", SegmentKind.POSITIONAL))
                tokens.append("")
            elif t == ">":
                if i != len(parts) - 1:
                    raise ValueError(f'pattern {p!r}: ">" must be the last token')
                segs.append(Segment("Rest", SegmentKind.REST))
                tokens.append("")
            elif t.startswith("{") and t.endswith("}"):
                name = t[1:-1]
                if not _IDENT.match(name):
                    raise ValueError(f"pattern {p!r}: segment name {name!r} is not an identifier")
                if name in seen:
                    raise ValueError(f"pattern {p!r}: segment {name!r} repeats")
                seen.add(name)
                segs.append(Segment(name, SegmentKind.NAMED))
                tokens.append("")
            else:
                if set(t) & _BAD_TOKEN_CHARS:
                    raise ValueError(f"pattern {p!r}: bad token {t!r}")
                tokens.append(t)
        return cls(p, tokens, segs)

    def __str__(self) -> str:
        return self._raw

    def __repr__(self) -> str:
        return f"Pattern({self._raw!r})"

    @property
    def segments(self) -> list[Segment]:
        return list(self._segs)

    def wildcard(self) -> str:
        """The NATS subject filter: "{name}" becomes "*"."""
        out: list[str] = []
        si = 0
        for t in self._tokens:
            if t != "":
                out.append(t)
                continue
            out.append(">" if self._segs[si].kind is SegmentKind.REST else "*")
            si += 1
        return ".".join(out)

    def subject(self, values: Sequence[str]) -> str:
        """Fill the wildcard segments, in order, and return the
        concrete subject."""
        if len(values) != len(self._segs):
            raise ValueError(f"pattern {self._raw!r}: {len(values)} segment values, want {len(self._segs)}")
        out: list[str] = []
        si = 0
        for t in self._tokens:
            if t != "":
                out.append(t)
                continue
            v, seg = values[si], self._segs[si]
            si += 1
            if seg.kind is SegmentKind.REST:
                if not all(_valid_token(rt) for rt in v.split(".")):
                    raise ValueError(f"pattern {self._raw!r}: segment {seg.name} = {v!r} is not a subject tail")
            elif not _valid_token(v):
                raise ValueError(f"pattern {self._raw!r}: segment {seg.name} = {v!r} is not a subject token")
            out.append(v)
        return ".".join(out)

    def match(self, subject: str) -> list[str] | None:
        """The wildcard segment values in order, or None when subject
        does not match."""
        parts = subject.split(".")
        vals: list[str] = []
        si = 0
        for i, t in enumerate(self._tokens):
            if i >= len(parts):
                return None
            if t != "":
                if parts[i] != t:
                    return None
                continue
            if self._segs[si].kind is SegmentKind.REST:
                vals.append(".".join(parts[i:]))
                return vals
            vals.append(parts[i])
            si += 1
        if len(parts) != len(self._tokens):
            return None
        return vals


def stream_name(key: str) -> str:
    """"EV_" + key with "." replaced by "_"."""
    return "EV_" + key.replace(".", "_")


def dlq_subject(stream: str) -> str:
    return DLQ_PREFIX + stream


@dataclass(frozen=True, slots=True)
class Channel:
    """One named channel."""

    key: str  # "<proto package>.<name>", e.g. "orders.v1.OrderEvents"
    pattern: Pattern
    stream: str  # stream (or topic) name: stream_name(key)

    @classmethod
    def new(cls, key: str, pattern: str) -> Channel:
        if key == "":
            raise ValueError("channel key is empty")
        try:
            p = Pattern.parse(pattern)
        except ValueError as e:
            raise ValueError(f"channel {key}: {e}") from None
        return cls(key=key, pattern=p, stream=stream_name(key))


_GROUP = re.compile(r"[A-Za-z0-9_-]+\Z")


def valid_group(group: str) -> bool:
    """Letters, digits, "_" and "-"."""
    return _GROUP.match(group) is not None
