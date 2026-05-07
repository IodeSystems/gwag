#!/usr/bin/env bash
# Runtime mutator for the bench stack: add or remove gateway nodes
# and backend services without bouncing the whole world. State is
# the per-node .env files in .run/{gateways,backends}; Prometheus's
# scrape set tracks live gateways via the file_sd refresh.
#
# Subcommands:
#   add-gateway [--name NAME]
#   add-backend KIND [--version vN] [--gateway NAME] [--name NAME]
#       KIND: greeter
#   rm NAME
#   status
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck disable=SC1091
source "$SCRIPT_DIR/lib.sh"

usage() {
    sed -n '3,/^set -euo pipefail/p' "${BASH_SOURCE[0]}" | grep '^# ' | sed 's/^# //'
    exit "${1:-1}"
}

cmd_add_gateway() {
    ensure_dirs
    local name=""
    while (($#)); do
        case $1 in
            --name) name=$2; shift 2 ;;
            -h|--help) usage 0 ;;
            *) echo "unknown arg: $1" >&2; usage ;;
        esac
    done
    # Port allocation tracks the count of existing gateway envs so
    # ports stay stable across re-runs even if the user picks
    # arbitrary names.
    local idx; idx=$(next_index "$GW_DIR" "n")
    if [[ -z $name ]]; then
        name="n$idx"
    fi
    if [[ -e $GW_DIR/$name.env ]]; then
        echo "gateway $name already registered" >&2
        return 1
    fi
    local http=$((HTTP_BASE + idx - 1))
    local cp=$((CP_BASE + idx - 1))
    local nats_listen=$((NATS_LISTEN_BASE + idx - 1))
    local nats_cluster=$((NATS_CLUSTER_BASE + idx - 1))
    local data="$NATS_DIR/$name"
    mkdir -p "$data"

    # NATS peer list = every existing live gateway's cluster port.
    local peers; peers=$(nats_peer_args)

    # Reserve the env file up front (with PID=0) so the port slot is
    # claimed before we exec — keeps concurrent invocations from
    # racing on the same idx if the user double-fires.
    cat > "$GW_DIR/$name.env" <<EOF
NAME=$name
HTTP_PORT=$http
CP_PORT=$cp
NATS_LISTEN=$nats_listen
NATS_CLUSTER=$nats_cluster
DATA=$data
PID=0
EOF

    echo "==> launching gateway $name (http=:$http control-plane=:$cp nats=:$nats_listen)"
    # shellcheck disable=SC2086
    "$BIN_DIR/gateway" \
        --node-name "$name" \
        --http ":$http" --control-plane ":$cp" \
        --nats-listen ":$nats_listen" --nats-cluster ":$nats_cluster" \
        --nats-data "$data" \
        $peers \
        > "$LOG_DIR/$name.log" 2>&1 &
    local pid=$!

    # Wait up to 10s for control plane to bind.
    for _ in $(seq 1 50); do
        nc -z 127.0.0.1 "$cp" 2>/dev/null && break
        sleep 0.2
    done

    sed -i "s/^PID=.*/PID=$pid/" "$GW_DIR/$name.env"
    write_targets
    echo "    gateway $name up at http://localhost:$http/api/graphql"
}

cmd_add_backend() {
    ensure_dirs
    local kind=${1:-}
    [[ -n $kind ]] || { echo "missing KIND" >&2; usage; }
    shift
    local name="" version="v1" gateway=""
    while (($#)); do
        case $1 in
            --name) name=$2; shift 2 ;;
            --version) version=$2; shift 2 ;;
            --gateway) gateway=$2; shift 2 ;;
            -h|--help) usage 0 ;;
            *) echo "unknown arg: $1" >&2; usage ;;
        esac
    done
    if [[ $kind != greeter ]]; then
        echo "unsupported KIND $kind (only 'greeter' for now)" >&2
        return 1
    fi
    local idx; idx=$(next_index "$BE_DIR" "g")
    if [[ -z $name ]]; then
        name="g$idx"
    fi
    if [[ -e $BE_DIR/$name.env ]]; then
        echo "backend $name already registered" >&2
        return 1
    fi
    if [[ -z $gateway ]]; then
        gateway=$(pick_gateway)
        [[ -n $gateway ]] || { echo "no live gateway to bind against (start one first)" >&2; return 1; }
    fi
    local gw_env="$GW_DIR/$gateway.env"
    [[ -e $gw_env ]] || { echo "gateway $gateway not registered" >&2; return 1; }
    local cp_port; cp_port=$(load_env "$gw_env"; echo "$CP_PORT")
    local port=$((BE_BASE + idx - 1))

    cat > "$BE_DIR/$name.env" <<EOF
NAME=$name
PORT=$port
KIND=$kind
VERSION=$version
GATEWAY=$gateway
PID=0
EOF

    echo "==> launching backend $name ($kind $version → gateway $gateway)"
    "$BIN_DIR/$kind" \
        --addr ":$port" --advertise "localhost:$port" \
        --gateway "localhost:$cp_port" --version "$version" \
        > "$LOG_DIR/$name.log" 2>&1 &
    local pid=$!
    sed -i "s/^PID=.*/PID=$pid/" "$BE_DIR/$name.env"
    echo "    backend $name up on :$port"
}

cmd_rm() {
    local name=${1:-}
    [[ -n $name ]] || { echo "rm needs NAME" >&2; usage; }
    local f=""
    if [[ -e $GW_DIR/$name.env ]]; then
        f="$GW_DIR/$name.env"
    elif [[ -e $BE_DIR/$name.env ]]; then
        f="$BE_DIR/$name.env"
    else
        echo "$name not found" >&2
        return 1
    fi
    local pid; pid=$(load_env "$f"; echo "$PID")
    if proc_alive "$pid"; then
        kill "$pid" 2>/dev/null || true
        for _ in $(seq 1 10); do proc_alive "$pid" || break; sleep 0.2; done
        proc_alive "$pid" && kill -9 "$pid" 2>/dev/null || true
    fi
    rm -f "$f"
    write_targets
    echo "    $name removed"
}

cmd_status() {
    printf '%-8s %-12s %-30s %s\n' NAME KIND ENDPOINT PID
    for f in "$GW_DIR"/*.env "$BE_DIR"/*.env; do
        [[ -e $f ]] || continue
        (
            load_env "$f"
            local kind="gateway" endpoint=""
            if [[ $f == "$BE_DIR"/* ]]; then
                kind="$KIND/$VERSION"
                endpoint="localhost:$PORT → $GATEWAY"
            else
                endpoint="http://localhost:$HTTP_PORT/api/graphql"
            fi
            local marker="·"
            proc_alive "$PID" && marker="✓"
            printf '%-8s %-12s %-30s %s %s\n' "$NAME" "$kind" "$endpoint" "$PID" "$marker"
        )
    done
}

cmd=${1:-}
shift || true
case "$cmd" in
    add-gateway) cmd_add_gateway "$@" ;;
    add-backend) cmd_add_backend "$@" ;;
    rm)          cmd_rm "$@" ;;
    status)      cmd_status "$@" ;;
    -h|--help|"") usage 0 ;;
    *) echo "unknown subcommand: $cmd" >&2; usage ;;
esac
