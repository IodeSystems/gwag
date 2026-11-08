"""Shared helpers for the tests that need Go and a NATS server: a helper
process wrapper, the built Go helper, and a NATS URL."""

import os
import shutil
import subprocess
import threading
import time
from collections.abc import Callable, Iterator
from pathlib import Path

import pytest

HERE = Path(__file__).resolve().parent.parent
ROOT = HERE.parent.parent.parent

needs_go = pytest.mark.skipif(shutil.which("go") is None, reason="go not on PATH")


class Proc:
    """A helper process. A thread collects its stdout lines."""

    def __init__(self, argv: list[str], url: str, cwd: Path = HERE) -> None:
        self.p = subprocess.Popen(
            argv,
            cwd=cwd,
            env={**os.environ, "NATS_URL": url},
            stdin=subprocess.PIPE,
            stdout=subprocess.PIPE,
            text=True,
        )
        self.out: list[str] = []
        threading.Thread(target=self._pump, daemon=True).start()

    def _pump(self) -> None:
        assert self.p.stdout is not None
        for line in self.p.stdout:
            self.out.append(line.rstrip("\n"))

    def wait(self, what: str, cond: Callable[[], bool], timeout: float = 30) -> None:
        deadline = time.monotonic() + timeout
        while not cond():
            assert self.p.poll() in (None, 0), f"{self.p.args!r} exited {self.p.returncode}: {self.out}"
            assert time.monotonic() < deadline, f"timed out waiting for {what}: {self.out}"
            time.sleep(0.02)

    def deliveries(self) -> list[str]:
        return [x for x in list(self.out) if x not in ("READY", "DONE")]

    def ready(self) -> None:
        self.wait("READY", lambda: "READY" in self.out)

    def finish(self) -> list[str]:
        """Close stdin, wait for DONE, return the delivery lines."""
        assert self.p.stdin is not None
        self.p.stdin.close()
        self.wait("DONE", lambda: "DONE" in self.out)
        assert self.p.wait(timeout=30) == 0
        return self.deliveries()

    def run_to_exit(self, timeout: float = 30) -> list[str]:
        """Wait for a process that exits by itself; return its lines."""
        assert self.p.wait(timeout=timeout) == 0, self.out
        time.sleep(0.05)  # let the pump thread drain
        return list(self.out)


@pytest.fixture(scope="session")
def go_bin(tmp_path_factory: pytest.TempPathFactory) -> str:
    if shutil.which("go") is None:
        pytest.skip("go not on PATH")
    out = str(tmp_path_factory.mktemp("go-interop") / "go-interop")
    subprocess.run(["go", "build", "-o", out, "./examples/events/go-interop"], cwd=ROOT, check=True)
    return out


@pytest.fixture(scope="session")
def nats_url(request: pytest.FixtureRequest) -> Iterator[str]:
    """$NATS_URL if set (e.g. docker run --rm -p 4222:4222
    nats:2.10-alpine -js), else the Go helper's embedded server."""
    if url := os.environ.get("NATS_URL"):
        yield url
        return
    go_bin: str = request.getfixturevalue("go_bin")
    srv = Proc([go_bin, "serve"], "")
    srv.wait("server url", lambda: bool(srv.out))
    yield srv.out[0]
    assert srv.p.stdin is not None
    srv.p.stdin.close()
    srv.p.wait(timeout=10)
