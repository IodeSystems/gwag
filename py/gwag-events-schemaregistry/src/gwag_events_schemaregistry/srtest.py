"""An in-memory Schema Registry for tests: the subset of Confluent's
REST API this package uses. Mirrors events/schemaregistry/srtest in Go.
Schemas compare as exact strings; there is no compatibility checking.

    reg = FakeRegistry()
    client = Client(Config(url="http://sr.test", http_client=reg.http_client()))
"""

from __future__ import annotations

import json
from urllib.parse import unquote

import httpx

from .client import Reference, Schema


def _same(a: Schema, b: Schema) -> bool:
    return a.schema == b.schema and a.references == b.references


class FakeRegistry:
    def __init__(self) -> None:
        self.requests: dict[str, int] = {}
        self._next = 1
        self._ids: dict[int, Schema] = {}
        self._subjects: dict[str, list[Schema]] = {}

    def http_client(self) -> httpx.AsyncClient:
        return httpx.AsyncClient(transport=httpx.MockTransport(self._serve))

    def add(self, subject: str, s: Schema) -> tuple[int, int]:
        """Register s under subject; return its id and version."""
        v = self._add(subject, s)
        return v.id, v.version

    def _add(self, subject: str, s: Schema) -> Schema:
        for v in self._subjects.get(subject, []):
            if _same(v, s):
                return v
        sid = next((i for i, v in self._ids.items() if _same(v, s)), 0)
        if sid == 0:
            sid = self._next
            self._next += 1
            self._ids[sid] = Schema(s.schema, "PROTOBUF", list(s.references))
        v = Schema(s.schema, "PROTOBUF", list(s.references), subject, len(self._subjects.get(subject, [])) + 1, sid)
        self._subjects.setdefault(subject, []).append(v)
        return v

    @staticmethod
    def _json(code: int, d: object) -> httpx.Response:
        return httpx.Response(code, json=d, headers={"Content-Type": "application/vnd.schemaregistry.v1+json"})

    @staticmethod
    def _out(s: Schema) -> dict[str, object]:
        d: dict[str, object] = {"schema": s.schema, "schemaType": s.schema_type, "id": s.id}
        if s.references:
            d["references"] = [{"name": r.name, "subject": r.subject, "version": r.version} for r in s.references]
        if s.subject:
            d["subject"] = s.subject
            d["version"] = s.version
        return d

    def _nf(self, code: int, msg: str) -> httpx.Response:
        return self._json(404, {"error_code": code, "message": msg})

    def _serve(self, req: httpx.Request) -> httpx.Response:
        parts = [unquote(p) for p in req.url.raw_path.decode().split("?")[0].strip("/").split("/")]
        self.requests[f"{req.method} /{'/'.join(parts)}"] = self.requests.get(f"{req.method} /{'/'.join(parts)}", 0) + 1
        body = Schema("")
        if req.method == "POST":
            body = Schema.from_json(json.loads(req.content))
        if req.method == "GET" and len(parts) == 3 and parts[:2] == ["schemas", "ids"]:
            s = self._ids.get(int(parts[2]))
            if s is None:
                return self._nf(40403, "Schema not found")
            s.id = int(parts[2])
            return self._json(200, self._out(s))
        if req.method == "GET" and len(parts) == 4 and parts[0] == "subjects" and parts[2] == "versions":
            vs = self._subjects.get(parts[1])
            if vs is None:
                return self._nf(40401, f"Subject '{parts[1]}' not found.")
            n = len(vs) if parts[3] == "latest" else int(parts[3])
            if n < 1 or n > len(vs):
                return self._nf(40402, f"Version {parts[3]} not found.")
            return self._json(200, self._out(vs[n - 1]))
        if req.method == "POST" and len(parts) == 3 and parts[0] == "subjects" and parts[2] == "versions":
            return self._json(200, {"id": self._add(parts[1], body).id})
        if req.method == "POST" and len(parts) == 2 and parts[0] == "subjects":
            vs = self._subjects.get(parts[1])
            if vs is None:
                return self._nf(40401, f"Subject '{parts[1]}' not found.")
            for v in vs:
                if _same(v, body):
                    return self._json(200, self._out(v))
            return self._nf(40403, "Schema not found")
        return self._nf(404, f"no route {req.method} {req.url.path}")


__all__ = ["FakeRegistry", "Reference"]
