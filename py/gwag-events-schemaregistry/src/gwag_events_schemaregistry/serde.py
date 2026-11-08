"""The Serde with Confluent's protobuf wire format. Mirrors
events/schemaregistry/serde.go."""

from __future__ import annotations

from collections.abc import Callable, Mapping
from dataclasses import dataclass

from gwag_events import HEADER_TYPE, Channel, Files, Message, default_files

from .client import Client
from .wire import MAGIC_BYTE, frame, unframe

# Set on a decoded message (not on the wire): the schema id the record
# was framed with.
HEADER_SCHEMA_ID = "Gwag-Schema-Id"

# Names the subject of a record's value schema: (topic, message full name).
SubjectNameStrategy = Callable[[str, str], str]


def topic_name_strategy(topic: str, full_name: str) -> str:
    """"<topic>-value". Confluent's default; one schema per topic."""
    return topic + "-value"


def record_name_strategy(topic: str, full_name: str) -> str:
    """The message full name. A topic may carry several types."""
    return full_name


def topic_record_name_strategy(topic: str, full_name: str) -> str:
    """"<topic>-<message full name>"."""
    return f"{topic}-{full_name}"


def strategy_by_name(name: str) -> SubjectNameStrategy:
    """"topic", "record" or "topic-record"."""
    match name:
        case "" | "topic" | "TopicNameStrategy":
            return topic_name_strategy
        case "record" | "RecordNameStrategy":
            return record_name_strategy
        case "topic-record" | "TopicRecordNameStrategy":
            return topic_record_name_strategy
    raise ValueError(f"subject name strategy {name!r}: want topic, record or topic-record")


@dataclass(frozen=True, slots=True)
class _Encoding:
    id: int
    indexes: list[int]


class Serde:
    """A gwag_events.Serde with Confluent framing. Publish frames the
    body with the schema id of the message's file; delivery accepts a
    framed record from any producer and sets Gwag-Type from the schema
    id and message indexes when the record has no Gwag-Type header. A
    record whose first byte is not the magic byte is taken as plain
    proto (plain proto never starts with 0), so a topic can move to
    framing without a cut-over.

    Options (the Go ones, in snake case):
      subject_name_strategy: default topic_name_strategy.
      auto_register: register the message's file (and its imports, as
        references) on first publish when the subject does not hold
        it. For protos gwag owns. Without it (the default) publish only
        looks the schema up and fails when it is not registered.
      use_latest: frame with the subject's latest schema, without
        comparing it to the local proto (Confluent's
        use.latest.version). The message must exist in that schema
        under the same full name.
      use_schema_id: frame every publish with this schema id. The
        message must exist in that schema under the same full name.
      normalize: normalize=true on lookups and registrations.
      reference_subject: names the subject of an imported file.
        Default: the import path.
      type_names: maps a registry message full name to the local one,
        for a shadow copy in another package: a record framed as
        "corp.orders.v1.Order" is delivered with Gwag-Type
        "shop.v1.OrderShadow". Applied on decode only.
      files: where generated code registered its file descriptors.
    """

    def __init__(
        self,
        client: Client,
        *,
        subject_name_strategy: SubjectNameStrategy = topic_name_strategy,
        auto_register: bool = False,
        use_latest: bool = False,
        use_schema_id: int | None = None,
        normalize: bool = False,
        reference_subject: Callable[[str], str] | None = None,
        type_names: Mapping[str, str] | None = None,
        files: Files = default_files,
    ) -> None:
        if sum([auto_register, use_latest, use_schema_id is not None]) > 1:
            raise ValueError("schema registry: auto_register, use_latest and use_schema_id are exclusive")
        self._c = client
        self._strategy = subject_name_strategy
        self._register = auto_register
        self._latest = use_latest
        self._id = use_schema_id
        self._normalize = normalize
        self._ref_subject = reference_subject
        self._type_names = dict(type_names or {})
        self._files = files
        self._ids: dict[tuple[str, str], _Encoding] = {}

    def subject(self, topic: str, full_name: str) -> str:
        """The subject a publish of full_name to topic uses."""
        return self._strategy(topic, full_name)

    async def encode(self, channel: Channel, topic: str, body: bytes, out: Message) -> None:
        name = out.headers.get(HEADER_TYPE, "")
        subject = self._strategy(topic, name)
        enc = await self._encoding(subject, name)
        out.data = frame(enc.id, enc.indexes, body)

    async def _encoding(self, subject: str, name: str) -> _Encoding:
        k = (subject, name)
        if not self._latest and (hit := self._ids.get(k)) is not None:
            return hit
        found = self._files.find_message(name)
        if self._id is None and not self._latest:
            if found is None:
                raise LookupError(f"no descriptor for {name}: generated code registers it (gwag gen --lang python)")
            info, indexes = found
            r = await self._c.ensure_file(
                subject,
                info.name,
                files=self._files,
                register=self._register,
                normalize=self._normalize,
                ref_subject=self._ref_subject,
            )
            enc = _Encoding(r.id, indexes)
        else:
            sid = self._id
            if self._latest:
                try:
                    sid = (await self._c.subject_version(subject, "latest")).id
                except Exception as e:
                    e.add_note(f"latest of {subject}")
                    raise
            assert sid is not None
            fi = await self._c.file_by_id(sid)
            idx = fi.indexes_of(name)
            if idx is None:
                raise LookupError(f"schema id {sid} (subject {subject}) has no message {name}")
            enc = _Encoding(sid, idx)
        if not self._latest:
            self._ids[k] = enc
        return enc

    async def decode(self, channel: Channel, topic: str, m: Message) -> None:
        if not m.data or m.data[0] != MAGIC_BYTE:
            return  # plain proto
        schema_id, indexes, body = unframe(m.data)
        fi = await self._c.file_by_id(schema_id)
        try:
            full = fi.message_at(indexes)
        except ValueError as e:
            raise ValueError(f"schema id {schema_id}: {e}") from None
        name = self._type_names.get(full, full)
        t = m.headers.get(HEADER_TYPE, "")
        if t == "":
            m.headers[HEADER_TYPE] = name
        elif t not in (name, full):
            raise ValueError(f"{HEADER_TYPE} header is {t!r} but schema id {schema_id} frames {full}")
        m.headers[HEADER_SCHEMA_ID] = str(schema_id)
        m.data = body
