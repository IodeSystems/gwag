"""Go <-> Python on one NATS JetStream server.

The Go half is examples/events/go-interop, the Python half is
interop.py. Both print the same line for the same message, so each
scenario compares lines. Needs `go` on PATH (the Go side is built from
the repo root). The server is $NATS_URL if set, else the Go helper's
embedded one (see conftest.py).
"""

import time
import uuid

from .conftest import Proc, needs_go

pytestmark = needs_go

CANONICAL = [
    "Audit         o-1002.refund: support refund",
    "OrderEvents   o-1001 created by ann",
    "OrderEvents   o-1002 cancelled: out of stock",
    "OrderShipped  o-1001 via ups",
    "Payments      eu/card captured 4200 for o-1001",
    "Payments      eu/card refunded 1500: cancelled",
    "Payments      us/gift note thank you",
]


def py(*args: str) -> list[str]:
    # The project's environment, whatever python started pytest.
    return ["uv", "run", "--locked", "python", "interop.py", *args]


def publish(argv: list[str], url: str) -> None:
    p = Proc(argv, url)
    p.wait("DONE", lambda: "DONE" in p.out)
    assert p.p.wait(timeout=30) == 0


def group(prefix: str) -> str:
    return f"{prefix}-{uuid.uuid4().hex[:8]}"


def test_go_publishes_python_listens(go_bin: str, nats_url: str) -> None:
    lis = Proc(py("listen", "--group", group("py")), nats_url)
    lis.ready()
    publish([go_bin, "publish"], nats_url)
    lis.wait("seven deliveries", lambda: len(lis.deliveries()) >= len(CANONICAL))
    assert sorted(lis.finish()) == CANONICAL


def test_python_publishes_go_listens(go_bin: str, nats_url: str) -> None:
    lis = Proc([go_bin, "listen", "-group", group("go")], nats_url)
    lis.ready()
    publish(py("publish"), nats_url)
    lis.wait("seven deliveries", lambda: len(lis.deliveries()) >= len(CANONICAL))
    assert sorted(lis.finish()) == CANONICAL


def test_same_group_shares_work_across_languages(go_bin: str, nats_url: str) -> None:
    g = group("shared")
    n = 20
    # Prefetch 1 and a pause per message so both consumers pull.
    go = Proc([go_bin, "listen", "-group", g, "-prefetch", "1", "-sleep-ms", "20"], nats_url)
    pyl = Proc(py("listen", "--group", g, "--prefetch", "1", "--sleep-ms", "20"), nats_url)
    go.ready()
    pyl.ready()
    publish([go_bin, "publish", "-no-canonical", "-bulk", str(n), "-prefix", "go"], nats_url)
    publish(py("publish", "--no-canonical", "--bulk", str(n), "--prefix", "py"), nats_url)
    go.wait(
        "all messages",
        lambda: len(go.deliveries()) + len(pyl.deliveries()) >= 2 * n,
        timeout=60,
    )
    time.sleep(0.5)  # a duplicate delivery would land in this window
    got_go, got_py = go.finish(), pyl.finish()
    want = sorted(f"OrderShipped  o-bulk-{p}-{i} via ups" for p in ("go", "py") for i in range(n))
    assert sorted(got_go + got_py) == want, "each message exactly once across the group"
    assert got_go and got_py, f"work not shared: go={len(got_go)} py={len(got_py)}"
