#!/usr/bin/env bash
# Compare the specs the orders code generates with the frozen
# snapshots in api/, or rewrite the snapshots.
#
#   ./snapshot.sh check                      # exit 1 unless every snapshot matches
#   ./snapshot.sh update                     # rewrite; refuses a breaking change
#   ./snapshot.sh update --allow-breaking    # rewrite anyway (retiring a version)
#
# The specs come from cmd/specs, which builds the service in process;
# no server runs. Comparison is `gwag ctl schema snapshot` (docs/lifecycle.md,
# "Frozen spec snapshots"), built from this checkout.
set -euo pipefail
cd "$(dirname "$0")"
HERE="$(pwd)"
ROOT="$(cd ../.. && pwd)"
verb="${1:-check}"
shift || true

# gw/cmd/gwag embeds ui/dist; seed it the way bin/build does.
if [[ ! -f "$ROOT/ui/dist/index.html" ]]; then
  mkdir -p "$ROOT/ui/dist"
  cp "$ROOT/ui/fallback/index.html" "$ROOT/ui/dist/index.html"
fi
mkdir -p .run
(cd "$ROOT" && go build -o "$HERE/.run/gwag" ./gw/cmd/gwag)
rm -rf .run/gen
go run ./cmd/specs -out .run/gen

# snapshot FORMAT GENERATED SNAPSHOT
fail=0
snapshot() {
  local rc=0
  .run/gwag ctl schema snapshot "$verb" --format "$1" --spec "$2" --snapshot "$3" "${@:4}" >.run/snap.out 2>&1 || rc=$?
  if [[ "$verb" == update ]]; then
    cat .run/snap.out
    [[ $rc -eq 0 ]] || fail=1
    return
  fi
  case $rc in
    0) echo "ok   $3" ;;
    3) cat .run/snap.out; echo "FAIL $3: additive change not in the snapshot; review, then ./snapshot.sh update" >&2; fail=1 ;;
    4) cat .run/snap.out; echo "FAIL $3: breaking change to a frozen version" >&2; fail=1 ;;
    *) cat .run/snap.out; echo "FAIL $3: gwag ctl schema snapshot exit $rc" >&2; fail=1 ;;
  esac
}

for v in v1 v2; do
  snapshot openapi ".run/gen/$v.openapi.json" "api/orders/$v/openapi.json" "$@"
  snapshot proto ".run/gen/$v.fds" "api/orders/$v/proto.json" "$@"
done
snapshot graphql .run/gen/schema.graphql api/orders/schema.graphql "$@"
exit $fail
