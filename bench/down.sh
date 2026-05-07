#!/usr/bin/env bash
# Tear the bench stack down: kill every process tracked in
# .run/{gateways,backends}/*.env, then docker compose down.
# Leaves built binaries + nats data on disk for cheap re-up; pass
# --purge to also wipe .run/.
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck disable=SC1091
source "$SCRIPT_DIR/lib.sh"

PURGE=0
for arg; do
    case $arg in
        --purge) PURGE=1 ;;
        *) echo "unknown arg: $arg" >&2; exit 1 ;;
    esac
done

if [[ -d $RUN_DIR ]]; then
    for dir in "$BE_DIR" "$GW_DIR"; do
        for f in "$dir"/*.env; do
            [[ -e $f ]] || continue
            (
                load_env "$f"
                if proc_alive "$PID"; then
                    echo "==> killing $NAME ($PID)"
                    kill "$PID" 2>/dev/null || true
                fi
            )
        done
    done
fi

echo "==> docker compose down"
(cd "$SCRIPT_DIR" && docker compose down) || true

if [[ $PURGE == 1 ]]; then
    echo "==> wiping $RUN_DIR"
    rm -rf "$RUN_DIR"
fi

echo "bench down."
