#!/usr/bin/env bash
# Run the gatbench matrix and render it as markdown.
#
#   compare/gatbench/report.sh            # 5 counts, ~2 min
#   COUNT=1 compare/gatbench/report.sh    # quick pass
#
# Writes compare/gatbench/results.md and rewrites the marked block in
# compare/README.md. Both are generated — edit this script, not them.
set -euo pipefail

cd "$(dirname "$0")"
root=$(cd ../.. && pwd)
count=${COUNT:-5}
out=results.md
raw=${RAW:-$(mktemp)}

echo "running gatbench matrix (count=$count)..." >&2
go test . -run '^$' -bench . -benchmem -benchtime=1s -count="$count" >"$raw"

# Median-of-counts would be better than mean, but `go test` already
# reports each count separately and the spread here is under 5%; a
# mean over counts is enough to keep the table stable run to run.
python3 render.py "$raw" "$out" "$root/README.md" "$root/compare/README.md"
echo "wrote $out" >&2
