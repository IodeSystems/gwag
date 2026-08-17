#!/usr/bin/env python3
"""Render `go test -bench` output for the gatbench matrix.

Writes results.md and rewrites the `<!-- BEGIN gat-matrix -->` and
`<!-- BEGIN gat-headline -->` blocks in any files passed after it.

    render.py <bench-output> <results.md> [file-with-marked-block ...]

Called by report.sh; not meant to be run by hand.
"""

import re
import statistics
import subprocess
import sys
from pathlib import Path

BEGIN = "<!-- BEGIN gat-matrix -->"
END = "<!-- END gat-matrix -->"
HEADLINE_BEGIN = "<!-- BEGIN gat-headline -->"
HEADLINE_END = "<!-- END gat-headline -->"

# Each row: (benchmark suffix, label, peer benchmark suffix, note).
# `peer` is the hand-written implementation this row is priced against;
# None marks the row as itself a baseline. Pairing per row rather than
# per surface is what lets each gRPC codec be compared against its own
# same-codec peer instead of against an average over callers.
MAIN_ROWS = [
    ("GatREST", "gat / REST", "GRPCGateway",
     "huma's own router — gat adds nothing to this path"),
    ("GRPCGateway", "grpc-gateway", None,
     "proto service + REST transcoding, in-process"),
    ("ConnectGoProto", "connect-go", None,
     "hand-written service, generated messages, binary codec"),
    ("GatConnectProto", "gat / Connect", "ConnectGoProto",
     "gat Connect handlers over the huma operation, binary codec"),
    ("Gqlgen", "gqlgen", None,
     "hand-written schema, generated resolvers"),
    ("GatGraphQL", "gat / GraphQL", "Gqlgen",
     "gat GraphQL ingress — plan cache + append-mode executor"),
]

# The gRPC surface serves several callers off one handler and they do
# not cost the same, so they get their own table rather than being
# averaged into a single number.
CODEC_ROWS = [
    ("ConnectGoProto", "connect-go", "Connect proto", None),
    ("GatConnectProto", "gat", "Connect proto", "ConnectGoProto"),
    ("ConnectGoGRPCWeb", "connect-go", "gRPC-Web", None),
    ("GatGRPCWeb", "gat", "gRPC-Web", "ConnectGoGRPCWeb"),
    ("ConnectGo", "connect-go", "Connect JSON", None),
    ("GatConnect", "gat", "Connect JSON", "ConnectGo"),
]

SCENARIOS = [("Get", "Point lookup"), ("List", "25-row list")]

CAVEATS = """#### Reading these tables

The peer is the hand-written implementation of the same surface,
speaking the same wire format: grpc-gateway for REST, connect-go for
gRPC, gqlgen for GraphQL.

- **gat / REST is huma, unmodified.** `gat.Register` calls
  `huma.Register` and captures a handler reference; it adds nothing to
  the REST path. That row compares huma's router against
  grpc-gateway's transcoder — useful, but it is not measuring gat.
- **The main table's gRPC row uses the binary codec**, which is what
  grpc-go and connect-go clients speak. The JSON codec costs both
  implementations substantially more; see the per-caller table.
- **gRPC proper (`application/grpc+proto`) is not measured here.** It
  needs HTTP/2, which an `httptest.ResponseRecorder` cannot provide,
  and standing up an h2c server would add a loopback round trip larger
  than the difference under test. It shares the binary codec path with
  the Connect-proto rows, plus HTTP/2 framing.
- **gqlgen runs with its query cache on**, as its own recommended
  server does, so neither GraphQL row re-parses per request."""


def parse(path):
    """benchmark name -> (mean ns/op, B/op, allocs/op) across counts."""
    pat = re.compile(
        r"^Benchmark(\S+?)(?:-\d+)?\s+\d+\s+([\d.]+) ns/op\s+(\d+) B/op\s+(\d+) allocs/op"
    )
    acc = {}
    for line in Path(path).read_text().splitlines():
        m = pat.match(line)
        if m:
            acc.setdefault(m.group(1), []).append(
                (float(m.group(2)), int(m.group(3)), int(m.group(4)))
            )
    return {
        name: (
            statistics.mean(v[0] for v in vals),
            round(statistics.mean(v[1] for v in vals)),
            round(statistics.mean(v[2] for v in vals)),
        )
        for name, vals in acc.items()
    }


def fmt_ns(ns):
    return f"{ns / 1000:.1f}µs" if ns >= 1000 else f"{ns:.0f}ns"


def cpu_model():
    for line in Path("/proc/cpuinfo").read_text().splitlines():
        if line.startswith("model name"):
            return line.split(":", 1)[1].strip()
    return "unknown"


def go_version():
    return subprocess.run(
        ["go", "version"], capture_output=True, text=True
    ).stdout.split()[2]


def ratio(data, prefix, suffix, peer):
    if peer is None:
        return "—"
    a, b = f"{prefix}_{suffix}", f"{prefix}_{peer}"
    if a not in data or b not in data:
        return "n/a"
    return f"{data[a][0] / data[b][0]:.2f}×"


def main_table(data, prefix):
    lines = [
        "| Implementation | ns/op | B/op | allocs/op | vs peer |",
        "|---|---:|---:|---:|---:|",
    ]
    for suffix, label, peer, _ in MAIN_ROWS:
        key = f"{prefix}_{suffix}"
        if key not in data:
            continue
        ns, b, allocs = data[key]
        lines.append(
            f"| {label} | {fmt_ns(ns)} | {b:,} | {allocs} |"
            f" {ratio(data, prefix, suffix, peer)} |"
        )
    return "\n".join(lines)


def codec_table(data, prefix):
    lines = [
        "| Caller | Implementation | ns/op | B/op | allocs/op | vs peer |",
        "|---|---|---:|---:|---:|---:|",
    ]
    for suffix, label, caller, peer in CODEC_ROWS:
        key = f"{prefix}_{suffix}"
        if key not in data:
            continue
        ns, b, allocs = data[key]
        lines.append(
            f"| {caller} | {label} | {fmt_ns(ns)} | {b:,} | {allocs} |"
            f" {ratio(data, prefix, suffix, peer)} |"
        )
    return "\n".join(lines)


def legend():
    return "\n".join(f"- **{label}** — {note}" for _, label, _, note in MAIN_ROWS)


def build_block(data):
    parts = [
        BEGIN,
        f"_Generated by `compare/gatbench/report.sh` on {cpu_model()}, {go_version()}._",
        "",
        "Same two operations, same in-memory store, same selection set —"
        " only the framework in front differs. Requests run through"
        " `ServeHTTP` against an `httptest.ResponseRecorder`: no sockets,"
        " no TLS. A loopback round trip costs ~300-400µs on this host and"
        " would bury every delta below.",
        "",
        "**Read the ratios, not the absolutes.** What carries to another"
        " machine is \"gat's Connect ingress costs N× a hand-written"
        " connect-go service on this workload\".",
        "",
    ]
    for prefix, title in SCENARIOS:
        parts += [f"### {title}", "", main_table(data, prefix), ""]
    parts += [
        "### gRPC surface, by caller",
        "",
        "One handler, several wire formats. Which one a client speaks"
        " changes how much serialization runs before the handler is"
        " reached, so a single \"gRPC\" number would average over callers"
        " whose costs differ.",
        "",
    ]
    for prefix, title in SCENARIOS:
        parts += [f"**{title}**", "", codec_table(data, prefix), ""]
    parts += [legend(), "", CAVEATS, "", END]
    return "\n".join(parts)


def build_headline(data):
    """Condensed three-row table for the root README — one row per
    surface, list scenario, gat against its hand-written peer."""
    rows = [
        ("REST", "GatREST", "GRPCGateway", "huma, unmodified", "grpc-gateway"),
        ("gRPC", "GatConnectProto", "ConnectGoProto", "binary codec", "connect-go"),
        ("GraphQL", "GatGraphQL", "Gqlgen", "", "gqlgen"),
    ]
    lines = ["| Surface | gat | hand-written | Ratio |", "|---|---:|---:|---:|"]
    for surface, gat_key, peer_key, gat_note, peer_name in rows:
        g = data[f"List_{gat_key}"][0]
        p = data[f"List_{peer_key}"][0]
        note = f" ({gat_note})" if gat_note else ""
        lines.append(
            f"| {surface} | {fmt_ns(g)}{note} | {fmt_ns(p)} ({peer_name}) |"
            f" {g / p:.2f}× |"
        )
    return "\n".join([
        HEADLINE_BEGIN,
        "\n".join(lines),
        "",
        "gat's REST path *is* huma — `gat.Register` wraps `huma.Register`",
        "and adds nothing to it. GraphQL comes out ahead of gqlgen: the",
        "executor walks a cached plan and appends response JSON straight",
        "into a pooled buffer, where gqlgen assembles an intermediate",
        "first. gRPC is the surface translation costs — gat allocates a",
        "dynamicpb message per element and walks it by reflection, where",
        "connect-go has generated structs. The ratio is worst on the",
        "binary codec shown here; a JSON-codec caller narrows it, but only",
        "because JSON penalises connect-go more than it penalises gat.",
        "Per-caller numbers and method:",
        "[`compare/gatbench/`](./compare/gatbench).",
        HEADLINE_END,
    ])


def splice(path, block, begin=BEGIN, end=END):
    p = Path(path)
    text = p.read_text()
    if begin not in text or end not in text:
        return False
    head, rest = text.split(begin, 1)
    _, tail = rest.split(end, 1)
    p.write_text(head + block + tail)
    return True


def main():
    raw, results = sys.argv[1], sys.argv[2]
    data = parse(raw)
    if not data:
        sys.exit("render: no benchmark lines parsed")
    block = build_block(data)

    Path(results).write_text(
        "# gatbench results\n\n"
        "Generated — run `compare/gatbench/report.sh` to refresh.\n\n"
        + block
        + "\n"
    )
    headline = build_headline(data)
    for target in sys.argv[3:]:
        if splice(target, block):
            print(f"spliced gat-matrix into {target}", file=sys.stderr)
        if splice(target, headline, HEADLINE_BEGIN, HEADLINE_END):
            print(f"spliced gat-headline into {target}", file=sys.stderr)


if __name__ == "__main__":
    main()
