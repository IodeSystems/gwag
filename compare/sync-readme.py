#!/usr/bin/env python3
"""Splice the headline matrix from comparison.md into compare/README.md.

comparison.md is written by `compare/cmd/compare`; this lifts its
"Headline matrix" section into the marked block in README.md so the
front page shows numbers instead of only a link.

    compare/sync-readme.py

Run it after a comparison sweep. The gat matrix has its own
generator — compare/gatbench/report.sh.
"""

import re
import sys
from pathlib import Path

BEGIN = "<!-- BEGIN gateway-matrix -->"
END = "<!-- END gateway-matrix -->"

HERE = Path(__file__).parent
COMPARISON = HERE / "comparison.md"
README = HERE / "README.md"


def main():
    text = COMPARISON.read_text()

    stamp = ""
    m = re.search(r"^_Generated ([^.]+)\.", text, re.M)
    if m:
        stamp = m.group(1)

    m = re.search(r"^## Headline matrix\s*\n(.*?)(?=^## )", text, re.M | re.S)
    if not m:
        sys.exit("sync-readme: no '## Headline matrix' section in comparison.md")
    # The Go renderer pads each row with a trailing space, which reads as
    # an extra empty column in some markdown viewers.
    table = "\n".join(line.rstrip() for line in m.group(1).strip().splitlines())

    block = "\n".join([
        BEGIN,
        f"_Generated {stamp} by `compare/cmd/compare`._" if stamp else "",
        "",
        "Each gateway runs the same sweep against the same backends on the"
        " same host, one at a time. Knee = highest rung where p99 stays"
        " under 50ms.",
        "",
        table,
        "",
        END,
    ])

    readme = README.read_text()
    if BEGIN not in readme or END not in readme:
        sys.exit(f"sync-readme: no marked block in {README}")
    head, rest = readme.split(BEGIN, 1)
    _, tail = rest.split(END, 1)
    README.write_text(head + block + tail)
    print(f"spliced {README}", file=sys.stderr)


if __name__ == "__main__":
    main()
