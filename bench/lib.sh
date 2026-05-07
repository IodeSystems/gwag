#!/usr/bin/env bash
# Shared helpers for bench/up.sh, bench/scale.sh, bench/down.sh.
# Sourced — every var defined here ends up in the caller's shell.

# Resolve repo + bench dirs from the script that sourced this file.
BENCH_DIR="$(cd "$(dirname "${BASH_SOURCE[1]}")" && pwd)"
REPO_DIR="$(cd "$BENCH_DIR/.." && pwd)"
RUN_DIR="$BENCH_DIR/.run"
BIN_DIR="$RUN_DIR/bin"
LOG_DIR="$RUN_DIR/logs"
GW_DIR="$RUN_DIR/gateways"
BE_DIR="$RUN_DIR/backends"
NATS_DIR="$RUN_DIR/nats"

# Port bases. Each kind allocates upward from its base.
HTTP_BASE=18080
CP_BASE=50090
NATS_LISTEN_BASE=14222
NATS_CLUSTER_BASE=14248
BE_BASE=15101

ensure_dirs() {
    mkdir -p "$BIN_DIR" "$LOG_DIR" "$GW_DIR" "$BE_DIR" "$NATS_DIR"
}

build_binaries() {
    ensure_dirs
    (cd "$REPO_DIR" && go build -o "$BIN_DIR/gateway" ./examples/multi/cmd/gateway)
    (cd "$REPO_DIR" && go build -o "$BIN_DIR/greeter" ./examples/multi/cmd/greeter)
    (cd "$REPO_DIR" && go build -o "$BIN_DIR/traffic" ./bench/cmd/traffic)
}

# build_binaries_if_missing only invokes the Go toolchain when one
# or more of the expected binaries doesn't already exist. Used by
# up.sh so a `bench restart` (which keeps .run/bin/ in place) skips
# the build entirely. `bin/bench up --build` is the explicit
# rebuild path that goes through bin/build.
build_binaries_if_missing() {
    ensure_dirs
    if [[ -x $BIN_DIR/gateway && -x $BIN_DIR/greeter && -x $BIN_DIR/traffic ]]; then
        return 0
    fi
    build_binaries
}

# next_index <dir> <prefix> — print the smallest unused integer N
# such that <dir>/<prefix>N.env doesn't exist. Used to allocate
# names like n1, n2, ... and g1, g2, ...
next_index() {
    local dir=$1 prefix=$2 i=1
    while [[ -e "$dir/$prefix$i.env" ]]; do
        i=$((i + 1))
    done
    echo "$i"
}

# proc_alive <pid> — 0 if running, non-zero otherwise.
proc_alive() {
    local pid=$1
    [[ -n $pid ]] && kill -0 "$pid" 2>/dev/null
}

# load_env <env-file> — source file in current shell. Each .env
# defines: NAME=, HTTP_PORT=, CP_PORT=, NATS_LISTEN=, NATS_CLUSTER=
# (gateway) or NAME=, PORT=, KIND=, VERSION=, GATEWAY= (backend).
# All scripts also share PID at the end.
load_env() {
    # shellcheck disable=SC1090
    source "$1"
}

# write_targets — rebuild .run/targets.json from every .env in
# gateways/. Prometheus picks the change up via file SD within
# refresh_interval. Output shape matches Prom's static_configs
# entries (an array of {labels, targets}).
write_targets() {
    ensure_dirs
    local out="$RUN_DIR/targets.json"
    local first=1
    {
        echo "["
        for f in "$GW_DIR"/*.env; do
            [[ -e $f ]] || continue
            (
                load_env "$f"
                local sep=","
                [[ $first == 1 ]] && sep=""
                printf '%s\n  {"labels": {"node": "%s"}, "targets": ["localhost:%s"]}' "$sep" "$NAME" "$HTTP_PORT"
            )
            first=0
        done
        echo
        echo "]"
    } > "$out"
}

# pick_gateway — print NAME of the first live gateway found, or ""
# if none. Used by add-backend when the user didn't specify one.
pick_gateway() {
    for f in "$GW_DIR"/*.env; do
        [[ -e $f ]] || continue
        local got
        got=$(
            load_env "$f"
            if proc_alive "$PID"; then
                echo "$NAME"
            fi
        )
        if [[ -n $got ]]; then
            echo "$got"
            return 0
        fi
    done
}

# nats_peer_args — print "--nats-peer 127.0.0.1:14248 [...]" for
# every live gateway's cluster port. Used when a NEW gateway joins;
# existing gateways auto-discover the new one once routes exchange.
nats_peer_args() {
    local args=""
    for f in "$GW_DIR"/*.env; do
        [[ -e $f ]] || continue
        (
            load_env "$f"
            if proc_alive "$PID"; then
                printf -- "--nats-peer 127.0.0.1:%s " "$NATS_CLUSTER"
            fi
        )
    done
}
