import pytest

from gwag_events import Backend, MemoryBackend, Message, testing
from gwag_events.testing import Harness


async def _new() -> Backend:
    return MemoryBackend()


async def _dead_letters(b: Backend, stream: str, n: int) -> list[Message]:
    assert isinstance(b, MemoryBackend)
    await testing.wait_for(f"{n} dead letters", lambda: len(b.dead_letters()) >= n)
    return b.dead_letters()


@pytest.mark.parametrize("name", list(testing.CHECKS))
async def test_conformance(name: str) -> None:
    await testing.CHECKS[name](Harness(new=_new, dead_letters=_dead_letters))
