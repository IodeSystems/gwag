"""Confluent Schema Registry for gwag_events: a client and a Serde with
Confluent's protobuf wire format (magic byte 0, a 4-byte big-endian
schema id, the message-index array, then the proto body). Python half of
events/schemaregistry in Go; the framing is byte for byte the same.

Who produces the topic decides how it is used (docs/events.md, "Schema
Registry"): own (gwag registers its protos), import (gwag reads a
pinned copy of another team's schema), shadow (gwag keeps its own copy
and `gwag schema check` compares it).

betterproto2 messages carry no descriptors, so `gwag gen --lang python`
embeds the serialized FileDescriptorProto of each file it generates
events for, and of each non-builtin import, in the `*_events.py` module.
They are the bytes the Go runtime registers.
"""

from .client import (
    Client,
    Config,
    NotRegisteredError,
    Reference,
    RegistryError,
    Schema,
    builtin,
    is_not_found,
)
from .serde import (
    HEADER_SCHEMA_ID,
    Serde,
    SubjectNameStrategy,
    record_name_strategy,
    strategy_by_name,
    topic_name_strategy,
    topic_record_name_strategy,
)
from .wire import MAGIC_BYTE, NotFramedError, frame, unframe

__all__ = [
    "HEADER_SCHEMA_ID",
    "MAGIC_BYTE",
    "Client",
    "Config",
    "NotFramedError",
    "NotRegisteredError",
    "Reference",
    "RegistryError",
    "Schema",
    "Serde",
    "SubjectNameStrategy",
    "builtin",
    "frame",
    "is_not_found",
    "record_name_strategy",
    "strategy_by_name",
    "topic_name_strategy",
    "topic_record_name_strategy",
    "unframe",
]
