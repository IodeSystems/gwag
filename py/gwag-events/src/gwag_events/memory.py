"""In-process Backend for tests. Semantics follow the JetStream
backend (and events/memory in Go): each group gets every message
published after the group's first subscriber (or every stored message
with deliver_all); subscribers sharing a group share its queue; failed
deliveries come back after the backoff; after max_deliver failures the
message goes to dead_letters(). Message ids dedup within a 2 minute
window. ack_wait is not modelled."""

from __future__ import annotations

import asyncio
import contextlib
import time
from collections.abc import Callable
from dataclasses import dataclass, field, replace

from .channel import Channel, dlq_subject
from .types import HEADER_ERROR, Handler, Message, Stop, SubscribeOptions, error_text

DEDUP_WINDOW = 120.0


@dataclass(slots=True)
class _Item:
    m: Message
    delivery: int


@dataclass(slots=True)
class _Group:
    queue: list[_Item] = field(default_factory=list)
    signal: asyncio.Event = field(default_factory=asyncio.Event)

    def push(self, it: _Item) -> None:
        self.queue.append(it)
        self.signal.set()


@dataclass(slots=True)
class _Channel:
    log: list[Message] = field(default_factory=list)
    groups: dict[str, _Group] = field(default_factory=dict)


def _clone(m: Message) -> Message:
    return replace(m, headers=dict(m.headers))


class MemoryBackend:
    def __init__(self, *, clock: Callable[[], float] = time.monotonic) -> None:
        self._channels: dict[str, _Channel] = {}
        self._seen: dict[str, float] = {}
        self._dlq: list[Message] = []
        self._tasks: set[asyncio.Task[None]] = set()
        self._timers: set[asyncio.TimerHandle] = set()
        self._closed = False
        self._clock = clock

    def _channel(self, ch: Channel) -> _Channel:
        c = self._channels.get(ch.stream)
        if c is None:
            c = self._channels[ch.stream] = _Channel()
        return c

    async def publish(self, channel: Channel, message: Message) -> None:
        if self._closed:
            raise RuntimeError("memory backend: closed")
        now = self._clock()
        if message.id:
            k = f"{channel.stream}\x00{message.id}"
            t = self._seen.get(k)
            if t is not None and now - t < DEDUP_WINDOW:
                return
            self._seen[k] = now
        stored = _clone(message)
        c = self._channel(channel)
        c.log.append(stored)
        for g in c.groups.values():
            g.push(_Item(stored, 1))

    async def subscribe(self, channel: Channel, group: str, opts: SubscribeOptions, handler: Handler) -> Stop:
        if self._closed:
            raise RuntimeError("memory backend: closed")
        c = self._channel(channel)
        g = c.groups.get(group)
        if g is None:
            g = c.groups[group] = _Group()
            if opts.deliver_all:
                for m in c.log:
                    g.push(_Item(m, 1))
        task = asyncio.get_running_loop().create_task(self._work(channel, g, opts, handler))
        self._tasks.add(task)
        task.add_done_callback(self._tasks.discard)

        async def stop() -> None:
            task.cancel()
            with contextlib.suppress(asyncio.CancelledError):
                await task

        return stop

    async def _work(self, ch: Channel, g: _Group, opts: SubscribeOptions, handler: Handler) -> None:
        while True:
            if not g.queue:
                g.signal.clear()
                await g.signal.wait()
                continue
            it = g.queue.pop(0)
            m = _clone(it.m)
            m.delivery = it.delivery
            try:
                await handler(m)
                continue
            except asyncio.CancelledError:
                # Stopped mid-delivery: leave the message for the group.
                g.queue.insert(0, it)
                raise
            except Exception as e:
                err = e
            if it.delivery >= opts.max_deliver:
                dead = _clone(it.m)
                dead.subject = dlq_subject(ch.stream)
                dead.headers[HEADER_ERROR] = error_text(err)
                dead.delivery = it.delivery
                self._dlq.append(dead)
                continue
            self._later(opts.delay(it.delivery), g, _Item(it.m, it.delivery + 1))

    def _later(self, delay: float, g: _Group, item: _Item) -> None:
        holder: list[asyncio.TimerHandle] = []

        def requeue() -> None:
            self._timers.discard(holder[0])
            if not self._closed:
                g.push(item)

        holder.append(asyncio.get_running_loop().call_later(delay, requeue))
        self._timers.add(holder[0])

    def dead_letters(self) -> list[Message]:
        """The dead-lettered messages, oldest first. Each has subject
        dlq_subject(stream) and the HEADER_ERROR header."""
        return [_clone(m) for m in self._dlq]

    async def close(self) -> None:
        """Stop every subscriber."""
        if self._closed:
            return
        self._closed = True
        for h in list(self._timers):
            h.cancel()
        self._timers.clear()
        tasks = list(self._tasks)
        for t in tasks:
            t.cancel()
        for t in tasks:
            with contextlib.suppress(asyncio.CancelledError):
                await t
