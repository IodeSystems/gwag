from __future__ import annotations

import asyncio
import socket
from collections.abc import AsyncIterator, Callable

import pytest
from grpclib.const import Status
from grpclib.exceptions import GRPCError
from grpclib.events import RecvRequest, listen
from grpclib.server import Server

from gwag_registration import (
    Dependency,
    Options,
    Owner,
    Service,
    Subscription,
    build_request,
    register,
)
from gwag_registration.gen.gateway.controlplane import v1 as cp

PROTO = 'syntax = "proto3"; package billing.v1;'
SVC = Service(
    namespace="billing",
    version="v1",
    proto_source=PROTO,
    subscribes=[Subscription("orders.v1.OrderEvents", "billing")],
)


class Fake(cp.ControlPlaneBase):
    """An in-process control plane: records calls, answers as the test says."""

    def __init__(self) -> None:
        self.registers: list[cp.RegisterRequest] = []
        self.heartbeats: list[str] = []
        self.deregisters: list[str] = []
        self.auth: list[str | None] = []
        self.known: set[str] = set()
        self.seq = 0
        self.ttl = 1
        self.state = cp.InstanceState.READY
        self.fail_register: GRPCError | None = None
        self.down = False  # answer UNAVAILABLE, as a gateway behind a dead load balancer
        self.server: Server | None = None
        self.port = 0

    def _status(self, req: cp.RegisterRequest) -> list[cp.BindingStatus]:
        out = []
        for s in req.services:
            depends = []
            if self.state == cp.InstanceState.WAITING:
                depends = [
                    cp.DependencyStatus(lines=["v1"], channel="orders.v1.OrderEvents", group="billing")
                ]
            out.append(cp.BindingStatus(namespace=s.namespace, version=s.version, state=self.state, depends=depends))
        return out

    async def register(self, message: cp.RegisterRequest) -> cp.RegisterResponse:
        if self.fail_register:
            raise self.fail_register
        self.registers.append(message)
        self.seq += 1
        rid = f"r{self.seq}"
        self.known.add(rid)
        return cp.RegisterResponse(registration_id=rid, ttl_seconds=self.ttl, bindings=self._status(message))

    async def heartbeat(self, message: cp.HeartbeatRequest) -> cp.HeartbeatResponse:
        if self.down:
            raise GRPCError(Status.UNAVAILABLE, "down")
        self.heartbeats.append(message.registration_id)
        ok = message.registration_id in self.known
        last = self.registers[-1] if self.registers else None
        return cp.HeartbeatResponse(ok=ok, bindings=self._status(last) if ok and last else [])

    async def deregister(self, message: cp.DeregisterRequest) -> cp.DeregisterResponse:
        self.deregisters.append(message.registration_id)
        self.known.discard(message.registration_id)
        return cp.DeregisterResponse()

    async def start(self, port: int = 0) -> str:
        self.server = Server([self])

        async def on_request(event: RecvRequest) -> None:
            self.auth.append(str(event.metadata.get("authorization")) if "authorization" in event.metadata else None)

        listen(self.server, RecvRequest, on_request)
        await self.server.start("127.0.0.1", port)
        self.port = next(iter(self.server._server.sockets)).getsockname()[1]  # type: ignore[union-attr]
        return f"127.0.0.1:{self.port}"

    async def stop(self) -> None:
        if self.server:
            self.server.close()
            try:
                await asyncio.wait_for(self.server.wait_closed(), 2)
            except TimeoutError:
                pass  # a client still holds its connection
            self.server = None


async def wait_for(cond: Callable[[], bool], timeout: float = 5.0) -> None:
    end = asyncio.get_running_loop().time() + timeout
    while not cond():
        if asyncio.get_running_loop().time() > end:
            raise TimeoutError("condition not met")
        await asyncio.sleep(0.02)


@pytest.fixture
async def fake() -> AsyncIterator[tuple[Fake, str]]:
    f = Fake()
    addr = await f.start()
    yield f, addr
    await f.stop()


def opts(addr: str | list[str], **kw: object) -> Options:
    kw.setdefault("logger", lambda m: None)
    return Options(gateway=addr, service_addr="billing:50051", services=[SVC], **kw)  # type: ignore[arg-type]


def test_build_request_carries_source_imports_and_subscribes() -> None:
    req = build_request(
        Options(
            gateway="g:1",
            service_addr="svc:1",
            instance_id="i",
            ttl_seconds=9,
            services=[
                Service(
                    namespace="n",
                    proto_source=PROTO,
                    proto_imports={"a.proto": "x"},
                    owner=Owner("t"),
                    depends=[Dependency("library", ["v1"])],
                    subscribes=[Subscription("orders.v1.OrderEvents", "billing")],
                )
            ],
        )
    )
    assert req.addr == "svc:1"
    assert req.ttl_seconds == 9
    b = req.services[0]
    assert b.proto_source == PROTO.encode()
    assert b.proto_imports == {"a.proto": b"x"}
    assert [(s.channel, s.group) for s in b.subscribes] == [("orders.v1.OrderEvents", "billing")]
    assert b.owner is not None and b.owner.team == "t"
    assert b.depends[0].lines == ["v1"]


def test_build_request_rejects_zero_or_several_sources() -> None:
    with pytest.raises(ValueError, match="exactly one"):
        build_request(Options(gateway="g:1", service_addr="s:1", services=[Service(namespace="x")]))
    with pytest.raises(ValueError, match="exactly one"):
        build_request(
            Options(gateway="g:1", service_addr="s:1", services=[Service(proto_source="a", openapi_spec="b")])
        )
    with pytest.raises(ValueError, match="at least one"):
        build_request(Options(gateway="g:1", service_addr="s:1", services=[]))


async def test_announces_heartbeats_and_deregisters(fake: tuple[Fake, str]) -> None:
    f, addr = fake
    reg = await register(opts(addr, heartbeat_interval=0.05))
    assert reg.id == "r1"
    assert f.auth[0] is None  # no key, no header
    assert f.registers[0].services[0].subscribes[0].channel == "orders.v1.OrderEvents"
    await wait_for(lambda: len(f.heartbeats) >= 2)
    await reg.close()
    assert f.deregisters == ["r1"]
    await reg.close()  # second close is a no-op
    assert len(f.deregisters) == 1


async def test_context_manager_deregisters(fake: tuple[Fake, str]) -> None:
    f, addr = fake
    async with await register(opts(addr)):
        pass
    assert f.deregisters == ["r1"]


async def test_heartbeat_period_defaults_to_ttl_over_three(fake: tuple[Fake, str]) -> None:
    f, addr = fake
    f.ttl = 1  # 0.33s period
    reg = await register(opts(addr))
    await wait_for(lambda: len(f.heartbeats) >= 2, 3.0)
    await reg.close()


async def test_registers_again_when_the_gateway_forgot(fake: tuple[Fake, str]) -> None:
    f, addr = fake
    reg = await register(opts(addr, heartbeat_interval=0.05))
    f.known.clear()  # gateway restarted
    await wait_for(lambda: len(f.registers) >= 2)
    assert reg.id == "r2"
    await reg.close()
    assert f.deregisters == ["r2"]


async def test_waits_for_ok_while_waiting(fake: tuple[Fake, str]) -> None:
    f, addr = fake
    f.state = cp.InstanceState.WAITING
    logs: list[str] = []
    task = asyncio.create_task(register(opts(addr, ready_poll_interval=0.03, logger=logs.append)))
    await wait_for(lambda: len(f.heartbeats) >= 2)
    assert not task.done()
    assert any("channel orders.v1.OrderEvents" in m for m in logs), logs
    f.state = cp.InstanceState.READY
    reg = await task
    assert reg.bindings[0].state == "ready"
    await reg.close()


async def test_refused_register_raises(fake: tuple[Fake, str]) -> None:
    f, addr = fake
    f.fail_register = GRPCError(Status.UNAUTHENTICATED, "bad key")
    with pytest.raises(GRPCError, match="bad key"):
        await register(opts(addr))


async def test_retries_an_unreachable_gateway_until_up() -> None:
    s = socket.socket()
    s.bind(("127.0.0.1", 0))
    port = s.getsockname()[1]
    s.close()
    f = Fake()
    logs: list[str] = []
    task = asyncio.create_task(register(opts(f"127.0.0.1:{port}", logger=logs.append)))
    await wait_for(lambda: len(logs) >= 1)
    await f.start(port)
    reg = await asyncio.wait_for(task, 10)
    assert "unreachable" in logs[0]
    await reg.close()
    await f.stop()


async def test_cancel_ends_the_wait() -> None:
    task = asyncio.create_task(register(opts("127.0.0.1:1")))
    await asyncio.sleep(0.1)
    task.cancel()
    with pytest.raises(asyncio.CancelledError):
        await task


async def test_moves_to_the_next_gateway() -> None:
    a, b = Fake(), Fake()
    aaddr, baddr = await a.start(), await b.start()
    reg = await register(opts([aaddr, baddr], heartbeat_interval=0.05))
    assert reg.gateway_addr == aaddr
    a.down = True
    await wait_for(lambda: len(b.registers) >= 1)
    assert reg.gateway_addr == baddr
    await reg.close()
    await a.stop()
    await b.stop()


async def test_application_key_is_sent_as_bearer_metadata(fake: tuple[Fake, str]) -> None:
    f, addr = fake
    reg = await register(opts(addr, application_key="app-key-1", heartbeat_interval=0.05))
    await wait_for(lambda: len(f.heartbeats) >= 1)
    await reg.close()
    assert f.auth and all(a == "Bearer app-key-1" for a in f.auth), f.auth
