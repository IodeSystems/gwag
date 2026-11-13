"""Service profile loader: the file that says what a service provides to gwag.

Format: gw/profile/profile.go and gw/profile/schema.json. Mirrors
``controlclient.LoadProfile`` (Go).

    services = load_profile("gwag.json", "billing:50051")
    reg = await register(Options(gateway=gw, service_addr="billing:50051", services=services))
"""

from __future__ import annotations

import importlib
import json
import re
from pathlib import Path
from typing import Any, NoReturn
from urllib.parse import urljoin

from .client import Dependency, Owner, Service, Subscription

__all__ = ["load_profile"]

_VERSION_RE = re.compile(r"^(?:v([1-9][0-9]*)|unstable)$")


def _version_key(v: str) -> tuple[int, str]:
    m = _VERSION_RE.match(v)
    return (int(m.group(1)) if m and m.group(1) else 1 << 30, v)


def _parse(path: Path) -> Any:
    text = path.read_text(encoding="utf-8")
    if path.suffix.lower() in (".yaml", ".yml"):
        try:
            yaml = importlib.import_module("yaml")
        except ImportError:
            raise ValueError(
                f"registration: profile {path}: reading YAML needs PyYAML; "
                "install it, or write gwag.json (gwag ctl services init -o gwag.json)"
            ) from None
        return yaml.safe_load(text)
    try:
        return json.loads(text)
    except json.JSONDecodeError as err:
        try:
            yaml = importlib.import_module("yaml")
        except ImportError:
            raise ValueError(
                f"registration: profile {path}: not JSON ({err}); reading YAML needs PyYAML: "
                "install it, or write gwag.json (gwag ctl services init -o gwag.json)"
            ) from None
        return yaml.safe_load(text)


def load_profile(path: str | Path, service_addr: str) -> list[Service]:
    """Read a service profile and return one Service per provided version.

    Sorted v1, v2, ..., unstable last. The spec is read from the file the
    profile names, relative to the profile's directory; proto imports are
    the other ``.proto`` files under the import directories. ``health_url``
    and ``spec_url`` are set so the gateway verifies each instance.
    ``service_addr`` is ``Options.service_addr``; a GraphQL endpoint given
    as a path is resolved against it.

    JSON always; YAML when PyYAML is importable. Raises ValueError for an
    invalid profile.
    """
    p = Path(path)
    prof = _parse(p)

    def fail(msg: str) -> NoReturn:
        raise ValueError(f"registration: profile {p}: {msg}")

    if not isinstance(prof, dict):
        fail("want a mapping at the top level")
    service = prof.get("service") or ""
    if not service:
        fail("service: required")
    provides: dict[str, Any] = prof.get("provides") or {}
    if not provides:
        fail("provides: at least one version required")
    base = p.parent

    def abs_(f: str) -> Path:
        q = Path(f)
        return q if q.is_absolute() else base / q

    depends = [
        Dependency(service=s, lines=list(prof["depends"][s])) for s in sorted(prof.get("depends") or {})
    ]
    subscribes = [Subscription(channel=s["channel"], group=s.get("group", "")) for s in prof.get("subscribes") or []]
    o = prof.get("owner")
    owner = Owner(team=o.get("team", ""), contact=o.get("contact", "")) if o else None

    out: list[Service] = []
    for v in sorted(provides, key=_version_key):
        pv: dict[str, Any] = provides[v] or {}
        where = f"provides.{v}"
        kind = pv.get("kind") or ""
        svc = Service(
            namespace=service,
            version=v,
            depends=depends,
            owner=owner,
            public=bool(prof.get("public", False)),
            subscribes=subscribes,
            health_url=pv.get("health") or prof.get("health") or "",
            spec_url=pv.get("url") or "",
        )
        if kind == "graphql":
            ep = pv.get("endpoint") or ""
            if not ep:
                fail(f"{where}.endpoint: required for kind graphql")
            if ep.startswith("/"):
                if not service_addr:
                    fail(f"{where}.endpoint {ep!r} is a path; pass the service address")
                addr = service_addr if "://" in service_addr else f"http://{service_addr}"
                ep = urljoin(addr, ep)
            svc.graphql_endpoint = ep
            svc.spec_url = ""
        elif kind in ("openapi", "proto"):
            file = pv.get("file") or ""
            if not file:
                fail(f"{where}.file: required for kind {kind}")
            entry = abs_(file)
            src = entry.read_bytes()
            if kind == "openapi":
                svc.openapi_spec = src
            else:
                svc.proto_source = src
                dirs = pv.get("imports") or [str(Path(file).parent)]
                imports: dict[str, str | bytes] = {}
                for d in dirs:
                    root = abs_(d)
                    if not root.is_dir():
                        continue
                    for f in sorted(root.rglob("*.proto")):
                        if f.resolve() == entry.resolve():
                            continue
                        imports.setdefault(f.relative_to(root).as_posix(), f.read_bytes())
                svc.proto_imports = imports
        elif kind == "":
            fail(f"{where}.kind: required: proto, openapi or graphql")
        else:
            fail(f"{where}.kind: {kind!r}: want proto, openapi or graphql")
        out.append(svc)
    return out
