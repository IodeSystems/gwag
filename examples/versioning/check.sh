#!/usr/bin/env bash
# Check examples/versioning without a gateway:
#
#   1. vet + tests: v1 contract goldens against the adapter, mapping
#      rules, applyDiscount sunset, both versions over GraphQL.
#   2. The goldens and the v1 snapshot still describe the legacy
#      service: record it again and compare.
#   3. Frozen snapshots: the specs the code generates equal api/.
#   4. Generated clients (Go from the proto snapshots, TypeScript from
#      the OpenAPI snapshots) equal what is committed.
#
#   examples/versioning/check.sh
#   WALK=1 examples/versioning/check.sh   # also run walkthrough.sh (a real gateway, ~2 min)
#
# Needs go; node >= 24 with pnpm for the TypeScript part. Generators
# are pinned by the projects: protoc-gen-go's generator at the root
# go.mod version (cmd/gen-go-client), openapi-typescript in
# ts/package.json. Nothing is taken from PATH.
set -euo pipefail
cd "$(dirname "$0")"
HERE="$(pwd)"
ROOT="$(cd ../.. && pwd)"
fail=0

# drift PATH...: fail if PATH differs from the commit or has new files.
drift() {
  if ! git diff --quiet -- "$@" || [[ -n "$(git ls-files --others --exclude-standard -- "$@")" ]]; then
    echo "FAIL regenerated output differs from committed:" >&2
    git status --short -- "$@" >&2
    fail=1
  else
    echo "ok   $* unchanged"
  fi
}

echo "== vet + tests"
(cd "$ROOT" && go vet ./examples/versioning/... && go test -count=1 ./examples/versioning/...)

echo "== goldens and v1 snapshot match the legacy service"
rm -rf .run/legacy
mkdir -p .run/legacy
go run ./cmd/legacy -record .run/legacy/contract
if diff -r .run/legacy/contract testdata/contract/v1; then
  echo "ok   testdata/contract/v1 = legacy answers"
else
  echo "FAIL legacy answers differ from testdata/contract/v1" >&2
  fail=1
fi
go run ./cmd/legacy -spec .run/legacy/openapi.json

echo "== frozen snapshots"
./snapshot.sh check || fail=1
rc=0
.run/gwag schema snapshot check --format openapi --spec .run/legacy/openapi.json \
  --snapshot api/orders/v1/openapi.json >.run/legacy/snap.out || rc=$?
if [[ $rc -eq 0 ]]; then
  echo "ok   legacy v1 spec = api/orders/v1/openapi.json"
else
  cat .run/legacy/snap.out
  echo "FAIL legacy v1 spec differs from api/orders/v1/openapi.json (exit $rc)" >&2
  fail=1
fi

echo "== Go clients from the proto snapshots"
rm -rf clients/go
go run ./cmd/gen-go-client -snapshot api/orders/v1/proto.json -pkg ordersv1 -out clients/go
go run ./cmd/gen-go-client -snapshot api/orders/v2/proto.json -pkg ordersv2 -out clients/go
drift clients/go

echo "== TypeScript clients from the OpenAPI snapshots"
(cd "$ROOT/ts/registration" && pnpm install --frozen-lockfile >/dev/null)
(cd ts && pnpm install --frozen-lockfile >/dev/null)
ts/gen.sh
drift ts/src/gen
(cd ts && pnpm typecheck >/dev/null) && echo "ok   ts typecheck" || { echo "FAIL ts typecheck" >&2; fail=1; }

if [[ "${WALK:-0}" == 1 ]]; then
  echo "== walkthrough"
  ./walkthrough.sh || fail=1
fi

if [[ $fail -ne 0 ]]; then
  echo "examples/versioning check FAILED" >&2
  exit 1
fi
echo "examples/versioning check passed"
