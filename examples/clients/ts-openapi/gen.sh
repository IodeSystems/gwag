#!/usr/bin/env bash
# Regenerate src/gen/ from the gateway's exported OpenAPI.
#
#   GATEWAY=http://localhost:8080 pnpm gen
set -euo pipefail
cd "$(dirname "$0")"
GATEWAY="${GATEWAY:-http://localhost:8080}"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
rm -rf src/gen
mkdir -p src/gen

for svc in greeter library; do
  # The export wraps each spec as {"<ns>": {"<version>": <spec>}}.
  # Take the v1 spec out of the wrapper.
  curl -fsS "$GATEWAY/api/schema/openapi?service=$svc" |
    node -e '
      const w = JSON.parse(require("fs").readFileSync(0, "utf8"));
      const [, vers] = Object.entries(w)[0];
      process.stdout.write(JSON.stringify(vers.v1));
    ' > "$tmp/$svc.json"
  pnpm exec openapi-typescript "$tmp/$svc.json" -o "src/gen/$svc.ts"
done
