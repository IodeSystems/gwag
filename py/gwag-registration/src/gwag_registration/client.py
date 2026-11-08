"""Light registration client for gwag's control plane.

Announce the service, wait for the gateway's ok, keep the registration
alive, deregister on close. Mirrors ``gw/controlclient`` (Go); see
docs/registration-clients.md.

    reg = await register(Options(
        gateway="gateway:50090",
        service_addr="billing:50051",
        services=[Service(namespace="billing", proto_source=PROTO,
                          subscribes=[Subscription("orders.v1.OrderEvents", "billing")])],
        application_key=os.environ.get("GWAG_APP_KEY"),
    ))
    ...
    await reg.close()
"""

from __future__ import annotations

import asyncio
import os
import signal as signal_mod
import sys
from collections.abc import Callable, Sequence
from dataclasses import dataclass, field
from typing import Literal
from urllib.parse import urlsplit

from grpclib.client import Channel
from grpclib.const import Status
from grpclib.exceptions import GRPCError, StreamTerminatedError

from .gen.gateway.controlplane import v1 as cp

__all__ = [
    "BindingStatus",
    "Dependency",
    "DependencyStatus",
    "Options",
    "Owner",
    "Registration",
    "Service",
    "Subscription",
    "build_request",
    "register",
]


@dataclass
class Subscription:
    """One event channel a binding listens on and the consumer group it listens as."""

    channel: str  # "<proto package>.<channel name>", e.g. "orders.v1.OrderEvents"
    group: str


@dataclass
class Dependency:
    """A provider service the binding calls through the gateway, with the lines it accepts."""

    service: str
    lines: list[str]  # "vN" or "unstable"


@dataclass
class Owner:
    team: str
    contact: str = ""


@dataclass
class Service:
    """One binding to register. Set exactly one of proto_source, openapi_spec, graphql_endpoint."""

    namespace: str = ""  # empty: the proto's file stem / the spec's title
    version: str = ""  # "unstable" or "vN"; empty: "v1"
    proto_source: str | bytes = b""  # raw .proto source of the entrypoint
    proto_imports: dict[str, str | bytes] = field(default_factory=dict)
    openapi_spec: str | bytes = b""  # service_addr is then the HTTP base URL
    graphql_endpoint: str = ""
    max_concurrency: int = 0
    max_concurrency_per_instance: int = 0
    depends: list[Dependency] = field(default_factory=list)
    owner: Owner | None = None
    public: bool = False
    subscribes: list[Subscription] = field(default_factory=list)


State = Literal["ready", "waiting", "degraded"]


@dataclass
class DependencyStatus:
    service: str
    lines: list[str]
    present_lines: list[str]
    channel: str
    group: str


@dataclass
class BindingStatus:
    namespace: str
    version: str
    state: State
    depends: list[DependencyStatus]


@dataclass
class Options:
    # Control-plane address: "host:port" (plain HTTP/2) or "http(s)://host:port".
    # Several addresses are tried in order; the registration lives on one
    # gateway at a time and moves to the next when a heartbeat cannot reach it.
    gateway: str | Sequence[str]
    service_addr: str  # host:port of this service's gRPC server (HTTP base URL for OpenAPI)
    services: Sequence[Service]
    # Sent as gRPC metadata ``authorization: Bearer <key>``.
    application_key: str | None = None
    instance_id: str = ""
    ttl_seconds: int = 0  # 0: the gateway default (30s)
    heartbeat_interval: float | None = None  # seconds; default ttl/3, or 10 when unknown
    ready_poll_interval: float = 1.0  # seconds, while a binding is WAITING
    # Deregister and exit on these signals (default none). The signal is
    # raised again after the deregister so the process exits as it would
    # have. Pass (signal.SIGTERM, signal.SIGINT) for a service with no
    # handler of its own; otherwise call ``close()`` from yours.
    deregister_on_signal: Sequence[signal_mod.Signals] = ()
    logger: Callable[[str], None] | None = None  # default: stderr


_RETRY_MIN = 0.25
_RETRY_MAX = 5.0
_HEARTBEAT_TIMEOUT_MAX = 5.0
_REGISTER_TIMEOUT = 10.0
_WAIT_LOG_EVERY = 30.0

_RETRYABLE = {Status.UNAVAILABLE, Status.DEADLINE_EXCEEDED, Status.RESOURCE_EXHAUSTED, Status.ABORTED}


def _retryable(err: BaseException) -> bool:
    if isinstance(err, GRPCError):
        return err.status in _RETRYABLE
    return isinstance(err, OSError | StreamTerminatedError | TimeoutError)


def _msg(err: BaseException) -> str:
    if isinstance(err, GRPCError):
        return err.message or str(err.status)
    return f"{type(err).__name__}: {err}"


def _bytes(v: str | bytes) -> bytes:
    return v.encode() if isinstance(v, str) else v


def build_request(opts: Options) -> cp.RegisterRequest:
    """The register request for opts: one binding per Service."""
    if not opts.services:
        raise ValueError("registration: at least one service required")
    bindings: list[cp.ServiceBinding] = []
    for s in opts.services:
        sources = [bool(s.proto_source), bool(s.openapi_spec), bool(s.graphql_endpoint)]
        if sum(sources) != 1:
            raise ValueError(
                f"service {s.namespace!r}: set exactly one of proto_source, openapi_spec, graphql_endpoint"
            )
        bindings.append(
            cp.ServiceBinding(
                namespace=s.namespace,
                version=s.version,
                proto_source=_bytes(s.proto_source),
                proto_imports={k: _bytes(v) for k, v in s.proto_imports.items()},
                openapi_spec=_bytes(s.openapi_spec),
                graphql_endpoint=s.graphql_endpoint,
                max_concurrency=s.max_concurrency,
                max_concurrency_per_instance=s.max_concurrency_per_instance,
                depends=[cp.Dependency(service=d.service, lines=list(d.lines)) for d in s.depends],
                owner=cp.Owner(team=s.owner.team, contact=s.owner.contact) if s.owner else None,
                public=s.public,
                subscribes=[cp.Subscription(channel=x.channel, group=x.group) for x in s.subscribes],
            )
        )
    return cp.RegisterRequest(
        addr=opts.service_addr,
        services=bindings,
        ttl_seconds=opts.ttl_seconds,
        instance_id=opts.instance_id,
    )


def _state(s: cp.InstanceState) -> State:
    if s == cp.InstanceState.WAITING:
        return "waiting"
    if s == cp.InstanceState.DEGRADED:
        return "degraded"
    return "ready"


def _bindings(items: list[cp.BindingStatus]) -> list[BindingStatus]:
    return [
        BindingStatus(
            namespace=b.namespace,
            version=b.version,
            state=_state(b.state),
            depends=[
                DependencyStatus(d.service, list(d.lines), list(d.present_lines), d.channel, d.group)
                for d in b.depends
            ],
        )
        for b in items
    ]


class _Gateway:
    def __init__(self, addr: str) -> None:
        self.addr = addr
        u = urlsplit(addr if "://" in addr else f"http://{addr}")
        host = u.hostname or "localhost"
        tls = u.scheme == "https"
        self.channel = Channel(host, u.port or (443 if tls else 80), ssl=tls or None)
        self.stub = cp.ControlPlaneStub(self.channel)


class Registration:
    """A live registration. Created by :func:`register`; ``close()`` deregisters."""

    def __init__(self, opts: Options) -> None:
        self._opts = opts
        self._log: Callable[[str], None] = opts.logger or (lambda m: print(m, file=sys.stderr))
        gateways = [opts.gateway] if isinstance(opts.gateway, str) else list(opts.gateway)
        addrs = list(dict.fromkeys(a for a in gateways if a))
        if not addrs:
            raise ValueError("registration: gateway address required")
        if not opts.service_addr:
            raise ValueError("registration: service_addr required")
        build_request(opts)  # validate now
        self._md: dict[str, str] = (
            {"authorization": f"Bearer {opts.application_key}"} if opts.application_key else {}
        )
        self._gws = [_Gateway(a) for a in addrs]
        self._cur = 0
        self._id = ""
        self._ttl = 0.0
        self._bindings: list[BindingStatus] = []
        self._task: asyncio.Task[None] | None = None
        self._closing: asyncio.Task[None] | None = None
        self._signals: list[signal_mod.Signals] = []
        self._sig_task: asyncio.Future[None] | None = None

    @property
    def id(self) -> str:
        """Registration id. Changes when the registration is made again."""
        return self._id

    @property
    def gateway_addr(self) -> str:
        """The control-plane address that holds the registration now."""
        return self._gws[self._cur].addr

    @property
    def bindings(self) -> list[BindingStatus]:
        """Start-gate state of each binding from the latest register or heartbeat."""
        return self._bindings

    def present_lines(self, service: str) -> list[str]:
        """Lines of ``service`` with a ready instance, from the bindings that depend on it."""
        out: dict[str, None] = {}
        for b in self._bindings:
            for d in b.depends:
                if d.service == service and not d.channel:
                    out.update(dict.fromkeys(d.present_lines))
        return list(out)

    async def __aenter__(self) -> Registration:
        return self

    async def __aexit__(self, *exc: object) -> None:
        await self.close()

    # -- protocol ----------------------------------------------------

    async def _register_once(self, timeout: float | None = None) -> None:
        resp = await self._gws[self._cur].stub.register(
            build_request(self._opts), timeout=timeout, metadata=self._md
        )
        self._id = resp.registration_id
        self._ttl = float(resp.ttl_seconds)
        self._bindings = _bindings(resp.bindings)

    async def _register_retry(self) -> None:
        """Register, retrying an unreachable gateway with backoff, forever."""
        backoff = _RETRY_MIN
        n = len(self._gws)
        attempt = 0
        while True:
            attempt += 1
            gw = self._gws[self._cur]
            try:
                await self._register_once(_REGISTER_TIMEOUT if n > 1 else None)
                return
            except asyncio.CancelledError:
                raise
            except Exception as err:
                if not _retryable(err):
                    raise
                if n > 1:
                    self._cur = (self._cur + 1) % n
                    if attempt % n != 0:
                        self._log(
                            f"registration: gateway {gw.addr} unreachable, trying {self._gws[self._cur].addr}: {_msg(err)}"
                        )
                        continue
                self._log(
                    f"registration: gateway {','.join(g.addr for g in self._gws)} unreachable, "
                    f"retrying in {backoff:g}s: {_msg(err)}"
                )
                await asyncio.sleep(backoff)
                backoff = min(backoff * 2, _RETRY_MAX)

    def _waiting(self) -> str:
        parts: list[str] = []
        for b in self._bindings:
            if b.state != "waiting":
                continue
            miss: list[str] = []
            for d in b.depends:
                if d.present_lines:
                    continue
                if d.channel and not d.service:
                    miss.append(f"channel {d.channel} (not declared yet)")
                elif d.channel:
                    miss.append(f"channel {d.channel} ({d.service}/{'|'.join(d.lines)})")
                else:
                    miss.append(f"{d.service}({'|'.join(d.lines)})")
            parts.append(f"{b.namespace}/{b.version} needs {', '.join(miss)}")
        return "; ".join(sorted(parts))

    async def _wait_ready(self) -> None:
        """Poll Heartbeat until no binding is WAITING."""
        loop = asyncio.get_running_loop()
        last_log, last_at = "", 0.0
        backoff = _RETRY_MIN
        while True:
            w = self._waiting()
            if not w:
                if last_log:
                    self._log("registration: dependencies present, starting")
                return
            if w != last_log or loop.time() - last_at >= _WAIT_LOG_EVERY:
                self._log(f"registration: waiting for dependencies: {w}")
                last_log, last_at = w, loop.time()
            await asyncio.sleep(self._opts.ready_poll_interval)
            gw = self._gws[self._cur]
            try:
                resp = await gw.stub.heartbeat(cp.HeartbeatRequest(registration_id=self._id), metadata=self._md)
            except asyncio.CancelledError:
                raise
            except Exception as err:
                self._log(f"registration: heartbeat to {gw.addr} while waiting: {_msg(err)}")
                if len(self._gws) > 1:
                    self._cur = (self._cur + 1) % len(self._gws)
                    await self._register_retry()
                    continue
                await asyncio.sleep(backoff)
                backoff = min(backoff * 2, _RETRY_MAX)
                continue
            backoff = _RETRY_MIN
            if not resp.ok:
                self._log("registration: gateway evicted us while waiting, re-registering")
                await self._register_retry()
                continue
            self._bindings = _bindings(resp.bindings)

    async def _start(self) -> None:
        try:
            await self._register_retry()
            await self._wait_ready()
        except BaseException:
            await self._deregister()
            await self._close_conns()
            raise
        o = self._opts
        interval = o.heartbeat_interval if o.heartbeat_interval is not None else (self._ttl / 3 if self._ttl > 0 else 10.0)
        self._task = asyncio.create_task(self._heartbeat_loop(interval))
        loop = asyncio.get_running_loop()
        for sig in o.deregister_on_signal:
            loop.add_signal_handler(sig, self._on_signal, sig)
            self._signals.append(sig)

    def _on_signal(self, sig: signal_mod.Signals) -> None:
        async def run() -> None:
            await self.close()
            signal_mod.signal(sig, signal_mod.SIG_DFL)
            os.kill(os.getpid(), sig)

        self._sig_task = asyncio.ensure_future(run())

    async def _heartbeat_loop(self, interval: float) -> None:
        timeout = min(interval, _HEARTBEAT_TIMEOUT_MAX)
        while True:
            await asyncio.sleep(interval)
            await self._beat(timeout)

    async def _beat(self, timeout: float) -> None:
        """One heartbeat. Not ok: register again on the same gateway. Unreachable: next gateway."""
        if not self._id:
            return
        gw = self._gws[self._cur]
        try:
            resp = await gw.stub.heartbeat(
                cp.HeartbeatRequest(registration_id=self._id), timeout=timeout, metadata=self._md
            )
            if resp.ok:
                self._bindings = _bindings(resp.bindings)
                return
            self._log(f"registration: gateway {gw.addr} does not know registration {self._id}, re-registering")
            try:
                await self._register_once(_REGISTER_TIMEOUT)
            except Exception as err:
                self._log(f"registration: re-register on {gw.addr} failed: {_msg(err)}")
                await self._failover(gw)
        except asyncio.CancelledError:
            raise
        except Exception as err:
            self._log(f"registration: heartbeat to {gw.addr}: {_msg(err)}")
            await self._failover(gw)

    async def _failover(self, failed: _Gateway) -> None:
        """Register on the addresses after ``failed``, in order, until one accepts. No-op with one."""
        n = len(self._gws)
        if n < 2:
            return
        start = self._gws.index(failed)
        for i in range(1, n):
            self._cur = (start + i) % n
            try:
                await self._register_once(_REGISTER_TIMEOUT)
            except asyncio.CancelledError:
                raise
            except Exception as err:
                self._log(f"registration: register on {self._gws[self._cur].addr} failed: {_msg(err)}")
                continue
            self._log(
                f"registration: registered on gateway {self._gws[self._cur].addr} instead of {failed.addr} "
                f"(registration {self._id})"
            )
            return

    async def _deregister(self) -> None:
        if not self._id:
            return
        try:
            await self._gws[self._cur].stub.deregister(
                cp.DeregisterRequest(registration_id=self._id), timeout=5.0, metadata=self._md
            )
        except Exception:  # best effort
            pass

    async def _close_conns(self) -> None:
        for g in self._gws:
            g.channel.close()

    async def close(self) -> None:
        """Stop the heartbeat and deregister. Safe to call more than once."""
        if self._closing is None:
            self._closing = asyncio.ensure_future(self._close())
        await asyncio.shield(self._closing)

    async def _close(self) -> None:
        loop = asyncio.get_running_loop()
        for sig in self._signals:
            loop.remove_signal_handler(sig)
        self._signals = []
        if self._task is not None:
            self._task.cancel()
            try:
                await self._task
            except asyncio.CancelledError:
                pass
        await self._deregister()
        await self._close_conns()


async def register(opts: Options) -> Registration:
    """Announce the service to the gateway and wait for its ok.

    - While the gateway is unreachable the register is retried with
      backoff, forever, and each failure is logged: the service does not
      start without the gateway. A refused register (bad request,
      dependency cycle, bad key) raises ``grpclib.exceptions.GRPCError``.
    - A binding whose depends (or subscribes) have no ready provider is
      WAITING; ``register`` polls Heartbeat and returns once the gateway
      reports every binding ready.
    - After that a task heartbeats at ttl/3, registers again when the
      gateway no longer knows the registration, and moves to the next
      gateway address when one is unreachable.

    Cancel the awaiting task to give up while waiting.
    """
    r = Registration(opts)
    await r._start()
    return r
