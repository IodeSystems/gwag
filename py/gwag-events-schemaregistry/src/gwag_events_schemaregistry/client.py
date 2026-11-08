"""A Confluent Schema Registry client. Mirrors the client half of
events/schemaregistry in Go. It uses httpx."""

from __future__ import annotations

import base64
import ssl
from collections.abc import Callable
from dataclasses import dataclass, field
from typing import Any
from urllib.parse import quote, urlsplit, urlunsplit

import httpx
from gwag_events import FileInfo, Files, default_files
from gwag_events.descriptors import parse_file

_ACCEPT = "application/vnd.schemaregistry.v1+json, application/json"
_CONTENT_TYPE = "application/vnd.schemaregistry.v1+json"

CODE_SUBJECT_NOT_FOUND = 40401
CODE_VERSION_NOT_FOUND = 40402
CODE_SCHEMA_NOT_FOUND = 40403


@dataclass(frozen=True, slots=True)
class Reference:
    """A schema reference: an import of the schema, registered under its
    own subject."""

    name: str  # the import path, e.g. "orders/v1/common.proto"
    subject: str
    version: int


@dataclass(slots=True)
class Schema:
    """A registered or candidate schema."""

    # .proto source, or base64 of a serialized FileDescriptorProto.
    schema: str
    schema_type: str = "PROTOBUF"
    references: list[Reference] = field(default_factory=list)
    # Set on responses.
    subject: str = ""
    version: int = 0
    id: int = 0

    def body(self) -> dict[str, Any]:
        out: dict[str, Any] = {"schema": self.schema, "schemaType": self.schema_type or "PROTOBUF"}
        if self.references:
            out["references"] = [{"name": r.name, "subject": r.subject, "version": r.version} for r in self.references]
        return out

    @classmethod
    def from_json(cls, d: dict[str, Any]) -> Schema:
        return cls(
            schema=d.get("schema", ""),
            schema_type=d.get("schemaType", "PROTOBUF"),
            references=[Reference(r["name"], r["subject"], r["version"]) for r in d.get("references", [])],
            subject=d.get("subject", ""),
            version=d.get("version", 0),
            id=d.get("id", 0),
        )


@dataclass(frozen=True, slots=True)
class Config:
    """How to reach a registry."""

    # The registry base URL, e.g. "https://sr.example.com:8081". User
    # info in the URL is used for basic auth.
    url: str
    # Basic auth.
    username: str = ""
    password: str = ""
    # Bearer token. Ignored when username is set.
    token: str = ""
    # HTTPS: False, a CA bundle path, or an ssl.SSLContext (custom CA,
    # client certificates). Ignored when http_client is set.
    verify: bool | str | ssl.SSLContext = True
    # Replaces the default httpx client. The client does not close it.
    http_client: httpx.AsyncClient | None = None


class RegistryError(Exception):
    """A registry error response."""

    def __init__(self, status: int, code: int, message: str) -> None:
        super().__init__(f"schema registry: {status} {message} (error_code {code})")
        self.status = status
        self.code = code
        self.message = message


class NotRegisteredError(Exception):
    """A lookup found no registered schema."""


def is_not_found(err: BaseException) -> bool:
    """Whether err is a registry "not found" (subject, version or
    schema)."""
    return isinstance(err, RegistryError) and (
        err.status == 404
        or err.code in (CODE_SUBJECT_NOT_FOUND, CODE_VERSION_NOT_FOUND, CODE_SCHEMA_NOT_FOUND)
    )


def builtin(path: str) -> bool:
    """Whether a registry knows the import path without a reference:
    google/protobuf/*, google/type/* and confluent/*."""
    return path.startswith(("google/protobuf/", "google/type/", "confluent/"))


def _seg(s: str) -> str:
    return quote(s, safe="")


class Client:
    """A Schema Registry client. It caches schemas by id and subject
    versions it has fetched by number; both are immutable in a
    registry. Close it with aclose()."""

    def __init__(self, cfg: Config) -> None:
        parts = urlsplit(cfg.url.rstrip("/"))
        if parts.scheme not in ("http", "https"):
            raise ValueError(f"schema registry url {cfg.url!r}: want http or https")
        username, password = cfg.username, cfg.password
        if parts.username and not username:
            username, password = parts.username, parts.password or ""
        host = parts.hostname or ""
        if ":" in host:
            host = f"[{host}]"
        netloc = host + (f":{parts.port}" if parts.port else "")
        self._base = urlunsplit((parts.scheme, netloc, parts.path, "", ""))
        self._auth: httpx.BasicAuth | None = httpx.BasicAuth(username, password) if username else None
        self._token = "" if username else cfg.token
        self._own = cfg.http_client is None
        self._http = cfg.http_client or httpx.AsyncClient(verify=cfg.verify)
        self._by_id: dict[tuple[int, str], Schema] = {}
        self._by_ver: dict[tuple[str, str, str], Schema] = {}
        self._files: dict[int, FileInfo] = {}
        self._ensured: dict[tuple[str, str, bool], Schema] = {}

    @property
    def url(self) -> str:
        """The registry URL without user info."""
        return self._base

    async def aclose(self) -> None:
        if self._own:
            await self._http.aclose()

    async def _do(self, method: str, path: str, *, query: dict[str, str] | None = None, body: Any = None) -> Any:
        headers = {"Accept": _ACCEPT}
        if body is not None:
            headers["Content-Type"] = _CONTENT_TYPE
        if self._token:
            headers["Authorization"] = "Bearer " + self._token
        try:
            resp = await self._http.request(
                method, self._base + path, params=query or None, json=body, headers=headers, auth=self._auth or httpx.USE_CLIENT_DEFAULT
            )
        except httpx.HTTPError as e:
            raise RuntimeError(f"schema registry: {method} {path}: {e}") from e
        if resp.status_code >= 300:
            code, msg = 0, resp.text.strip()
            try:
                d = resp.json()
                code, msg = int(d.get("error_code", 0)), d.get("message") or msg
            except ValueError:
                pass
            raise RegistryError(resp.status_code, code, f"{method} {path}: {msg}")
        return resp.json()

    async def schema_by_id(self, schema_id: int, format: str = "") -> Schema:
        """The schema with schema_id. format "" gives .proto text,
        "serialized" base64 of a FileDescriptorProto."""
        k = (schema_id, format)
        if (s := self._by_id.get(k)) is not None:
            return s
        s = Schema.from_json(
            await self._do("GET", f"/schemas/ids/{schema_id}", query={"format": format} if format else None)
        )
        s.id = schema_id
        self._by_id[k] = s
        return s

    async def subject_version(self, subject: str, version: str = "latest", format: str = "") -> Schema:
        """A version of subject: a number or "latest". Numbered versions
        are cached; "latest" is fetched every call."""
        version = version or "latest"
        k = (subject, version, format)
        if version != "latest" and (s := self._by_ver.get(k)) is not None:
            return s
        s = Schema.from_json(
            await self._do(
                "GET", f"/subjects/{_seg(subject)}/versions/{_seg(version)}", query={"format": format} if format else None
            )
        )
        if version != "latest":
            self._by_ver[k] = s
        return s

    async def register(self, subject: str, schema: Schema, normalize: bool = False) -> int:
        """Register schema under subject and return its id.
        Registering a schema the subject already holds returns the
        existing id."""
        out = await self._do(
            "POST",
            f"/subjects/{_seg(subject)}/versions",
            query={"normalize": "true"} if normalize else None,
            body=schema.body(),
        )
        return int(out["id"])

    async def lookup(self, subject: str, schema: Schema, normalize: bool = False) -> Schema:
        """The registered version of schema under subject (with its id
        and version), or a RegistryError is_not_found accepts."""
        return Schema.from_json(
            await self._do(
                "POST",
                f"/subjects/{_seg(subject)}",
                query={"normalize": "true"} if normalize else None,
                body=schema.body(),
            )
        )

    async def file_by_id(self, schema_id: int) -> FileInfo:
        """The file of schema schema_id: its package and message tree.
        Cached."""
        if (f := self._files.get(schema_id)) is not None:
            return f
        s = await self.schema_by_id(schema_id, "serialized")
        if s.schema_type not in ("", "PROTOBUF"):
            raise ValueError(f"schema id {schema_id} is {s.schema_type}, not PROTOBUF")
        try:
            f = parse_file(base64.b64decode(s.schema.strip(), validate=True))
        except ValueError as e:
            raise ValueError(f"schema id {schema_id}: not a serialized FileDescriptorProto: {e}") from e
        self._files[schema_id] = f
        return f

    async def ensure_file(
        self,
        subject: str,
        path: str,
        *,
        files: Files = default_files,
        register: bool = False,
        normalize: bool = False,
        ref_subject: Callable[[str], str] | None = None,
    ) -> Schema:
        """Find (or, with register, register) the file at path under
        subject, and each non-builtin import under its reference
        subject, imports first. The file and its imports come from
        files, where generated code registered them. Returns the
        registered root: subject, version, id."""
        ref_subject = ref_subject or (lambda p: p)
        k = (subject, path, register)
        if (hit := self._ensured.get(k)) is not None:
            return hit
        info = files.file(path)
        if info is None:
            raise LookupError(f"no descriptor for {path}: generated code registers it (gwag gen --lang python)")
        refs: list[Reference] = []
        for dep in info.dependency:
            if builtin(dep):
                continue
            r = await self.ensure_file(
                ref_subject(dep), dep, files=files, register=register, normalize=normalize, ref_subject=ref_subject
            )
            refs.append(Reference(dep, r.subject, r.version))
        s = Schema(schema=base64.b64encode(info.raw).decode(), references=refs)
        if register:
            try:
                await self.register(subject, s, normalize)
            except Exception as e:
                e.add_note(f"register {path} under {subject}")
                raise
        try:
            found = await self.lookup(subject, s, normalize)
        except RegistryError as e:
            if is_not_found(e):
                raise NotRegisteredError(f"{path} under subject {subject}: schema not registered ({e})") from e
            raise
        if not found.subject:
            found.subject = subject
        self._ensured[k] = found
        return found
