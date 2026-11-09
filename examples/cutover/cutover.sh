#!/usr/bin/env bash
# Runs a whole blue -> green cutover in Docker Compose with client
# traffic through Envoy the whole time, in the order of
# docs/cutover.md "Runbook", and checks:
#   - 0 failed client requests (else the steps where they failed);
#   - green holds blue's state after the copy;
#   - no event lost or delivered twice;
#   - switchover metrics: forwarded requests, no fallback, no green error.
#
#   ./cutover.sh             # build the images, run, clean up
#   ./cutover.sh --no-build  # use gwag:cutover and gwag-cutover-app as built
#   KEEP=1 ./cutover.sh      # leave the containers running at the end
#
# Needs docker (compose v2), curl, jq, openssl, bc. Host ports 28000-28192
# on 127.0.0.1.
set -euo pipefail
cd "$(dirname "$0")"

BUILD=1
[[ "${1:-}" == "--no-build" ]] && BUILD=0
RUN=.run
T0=$(date +%s.%N)

el() { printf '%6.1f' "$(echo "$(date +%s.%N) - $T0" | bc)"; }
log() { printf '[%ss] %s\n' "$(el)" "$*"; }
die() { printf '[%ss] FAIL: %s\n' "$(el)" "$*" >&2; exit 1; }
dc() { docker compose "$@"; }
dcq() { local out; out=$(docker compose "$@" 2>&1) || { printf '%s\n' "$out" >&2; return 1; }; }
cli() { docker compose run --rm -T --no-deps cli "$@" 2> >(grep -v -E '^ *(Container|Network|Volume) ' >&2); }

cleanup() {
	local code=$?
	if [[ "${KEEP:-0}" == 1 ]]; then
		log "KEEP=1: containers left running (docker compose -p gwag-cutover down -v to remove)"
	else
		log "cleaning up containers and volumes"
		dc --profile cli down -v --remove-orphans >/dev/null 2>&1 || true
	fi
	exit $code
}

blue_nodes=(blue-0 blue-1 blue-2)
green_nodes=(green-0 green-1 green-2)
port_of() { # host HTTP port of a node
	case $1 in
	blue-*) echo $((28180 + ${1#blue-})) ;;
	green-*) echo $((28190 + ${1#green-})) ;;
	esac
}
url_of() { echo "http://127.0.0.1:$(port_of "$1")"; }

wait_health() {
	local n
	for n in "$@"; do
		for _ in $(seq 1 120); do
			[[ "$(curl -s -o /dev/null -w '%{http_code}' "$(url_of "$n")/api/health")" == 200 ]] && continue 2
			sleep 0.5
		done
		die "$n not healthy after 60s"
	done
}

admin() { # admin NODE METHOD PATH [BODY]
	local tok=$BLUE_ADMIN_TOKEN
	[[ $1 == green-* ]] && tok=$GREEN_ADMIN_TOKEN
	local out code
	out=$(curl -s -w '\n%{http_code}' -X "$2" -H "Authorization: Bearer $tok" -H 'Content-Type: application/json' \
		${4:+--data "$4"} "$(url_of "$1")/api/admin$3")
	code=${out##*$'\n'}
	out=${out%$'\n'*}
	if [[ $code != 2* ]]; then
		printf '%s %s %s: HTTP %s %s\n' "$1" "$2" "$3" "$code" "$out" >&2
		return 1
	fi
	printf '%s\n' "$out"
}

retry() { # retry N CMD...: run CMD until it succeeds, at most N times, 1 s apart
	local n=$1 i
	shift
	for ((i = 1; i < n; i++)); do "$@" && return 0; sleep 1; done
	"$@"
}

gql() { # gql NODE BODY
	curl -s -H 'Content-Type: application/json' --data "$2" "$(url_of "$1")/api/graphql"
}

traffic() { curl -sf http://127.0.0.1:28100/stats; }
app() { curl -sf -X "${2:-GET}" "http://127.0.0.1:28101$1"; }

# metric NAME_REGEX: sum over blue nodes of the matching samples.
blue_metric() {
	local n sum=0 v
	for n in "${blue_nodes[@]}"; do
		v=$(curl -s "$(url_of "$n")/api/metrics" | grep -E "^[a-z_]*$1" | awk '{s+=$2} END {print s+0}')
		sum=$(echo "$sum + $v" | bc)
	done
	echo "$sum"
}

STEP_NAMES=()
STEP_SECS=()
STEP_FAILS=()
STEP_REQS=()
STEP_T=0
STEP_F=0
STEP_R=0
step_begin() {
	STEP_NAME=$1
	STEP_T=$(date +%s.%N)
	local s
	s=$(traffic)
	STEP_F=$(jq .failed <<<"$s")
	STEP_R=$(jq .total <<<"$s")
	echo
	log "=== $STEP_NAME"
}
step_end() {
	local s f r
	s=$(traffic)
	f=$(jq .failed <<<"$s")
	r=$(jq .total <<<"$s")
	STEP_NAMES+=("$STEP_NAME")
	STEP_SECS+=("$(printf '%.1f' "$(echo "$(date +%s.%N) - $STEP_T" | bc)")")
	STEP_FAILS+=($((f - STEP_F)))
	STEP_REQS+=($((r - STEP_R)))
	log "client requests in this step: $((r - STEP_R)), failed: $((f - STEP_F))"
}

# ---------------------------------------------------------------- setup
trap cleanup EXIT
if [[ $BUILD == 1 ]]; then
	log "building gwag:cutover (repo Dockerfile) and gwag-cutover-app"
	docker build -q -t gwag:cutover ../.. >/dev/null
	docker build -q -t gwag-cutover-app -f Dockerfile ../.. >/dev/null
	log "images built"
fi
dc --profile cli down -v --remove-orphans >/dev/null 2>&1 || true
rm -rf "$RUN"
mkdir -p "$RUN/keys"
chmod 755 "$RUN" "$RUN/keys"
BLUE_ADMIN_TOKEN=$(openssl rand -hex 32)
GREEN_ADMIN_TOKEN=$(openssl rand -hex 32)
(
	umask 077
	echo "GWAG_ADMIN_TOKEN=$BLUE_ADMIN_TOKEN" >"$RUN/blue.env"
	echo "GWAG_ADMIN_TOKEN=$GREEN_ADMIN_TOKEN" >"$RUN/green.env"
)

log "starting blue (3 nodes) and envoy"
dcq up -d "${blue_nodes[@]}" envoy
wait_health "${blue_nodes[@]}"
log "blue healthy; starting app (registers with blue, consumer and producer on blue)"
dcq up -d app
for _ in $(seq 1 60); do app /status >/dev/null 2>&1 && break; sleep 0.5; done

# Blue mints the system key green's mirror connection presents.
cli key create --system --ttl 1d --note "green mirror" --endpoint http://blue-0:8080 --bearer "$BLUE_ADMIN_TOKEN" 2>/dev/null | head -1 >"$RUN/keys/mirror.key"
chmod 644 "$RUN/keys/mirror.key"
grep -q '^gws_' "$RUN/keys/mirror.key" || die "mirror key not minted"
log "mirror key minted on blue: $(cut -c1-21 "$RUN/keys/mirror.key")..."

for _ in $(seq 1 60); do
	gql blue-0 '{"query":"{ items { listItems { id } } }"}' | jq -e '.data.items.listItems' >/dev/null 2>&1 && break
	sleep 0.5
done
gql blue-0 '{"query":"{ items { listItems { id } } }"}' | jq -e '.data.items.listItems' >/dev/null || die "items not served on blue"

# Operator state on blue, for the state copy to carry: an app key, a
# deprecation, an external dependent.
cli key create --ns items --ttl 30d --note "items service" --endpoint http://blue-0:8080 --bearer "$BLUE_ADMIN_TOKEN" >/dev/null 2>&1
retry 10 admin blue-0 POST /services/items/v1/deprecate '{"reason":"items v2 is coming"}' >/dev/null
retry 10 admin blue-0 PUT /external-dependents/acme '{"name":"Acme","kind":"customer","depends":[{"service":"items","lines":["v1"]}],"reviewAt":"2027-06-01T00:00:00Z"}' >/dev/null
log "blue state: app key, deprecation of items/v1, external dependent acme"

log "starting green (3 nodes) with --mirror-from blue"
dcq up -d "${green_nodes[@]}"
wait_health "${green_nodes[@]}"
log "green healthy; app registers with green"
app /register-green POST >/dev/null
for _ in $(seq 1 60); do
	gql green-0 '{"query":"{ items { listItems { id } } }"}' | jq -e '.data.items.listItems' >/dev/null 2>&1 && break
	sleep 0.5
done
gql green-0 '{"query":"{ items { listItems { id } } }"}' | jq -e '.data.items.listItems' >/dev/null || die "items not served on green"
log "app registered with both clusters"

dcq up -d traffic
for _ in $(seq 1 60); do traffic >/dev/null 2>&1 && break; sleep 0.5; done
sleep 3
log "traffic running: $(traffic | jq -c '{total, failed, byCluster}')"

# ------------------------------------------------- 1. replay verification
step_begin "1. replay verify: system key on green, blue records, green replays"
replay_counts() {
	local n
	for n in "${blue_nodes[@]}"; do admin "$n" GET /replay/diffs | jq -c .counts; done |
		jq -s -c '{match: (map(.match)|add), diff: (map(.diff)|add), unverifiable: (map(.unverifiable)|add), dropped: (map(.dropped)|add), error: (map(.error)|add)}'
}
log "before the key (posts to green are refused, outcome error): $(replay_counts)"
cli key create --system --ttl 1d --note "blue replay verify" --endpoint http://green-0:8080 --bearer "$GREEN_ADMIN_TOKEN" 2>/dev/null | head -1 >"$RUN/keys/.green-system.key"
chmod 644 "$RUN/keys/.green-system.key"
mv "$RUN/keys/.green-system.key" "$RUN/keys/green-system.key"
R0=$(replay_counts)
sleep 10
R1=$(replay_counts)
log "replay outcomes in 10 s on blue (sum of 3 nodes): $(jq -n -c --argjson a "$R0" --argjson b "$R1" '$b | with_entries(.value -= $a[.key])')"
DIFFS=$(for n in "${blue_nodes[@]}"; do admin "$n" GET /replay/diffs | jq -c '.items[] | select(.outcome != "error")'; done)
echo "$DIFFS" | jq -s -c 'group_by(.operationName + " " + .outcome) | map({op: .[0].operationName, outcome: .[0].outcome, n: length, diffs: (map(.diffs[]? | "\(.path): \(.blue) -> \(.green)") | unique)})[]'
MATCH=$(jq -n --argjson a "$R0" --argjson b "$R1" '$b.match - $a.match')
NDIFF=$(jq -n --argjson a "$R0" --argjson b "$R1" '$b.diff - $a.diff')
OTHER=$(echo "$DIFFS" | jq -s '[.[] | select(.operationName != "GetItem" or (.diffs | map(.path) != ["legs[0].request.path"]))] | length')
((MATCH > 0)) || die "no replay matched"
((NDIFF > 0)) || die "no replay diff (expected GetItem's path)"
((OTHER == 0)) || die "unexpected replay diffs: $OTHER"
log "replay: ListItems and AddNote match; GetItem differs only in legs[0].request.path (the intended spec change)"
step_end

# --------------------------------------------------------- 2. state copy
step_begin "2. state copy: freeze blue, export, import into green, unfreeze"
cli cluster freeze --gateway http://blue-0:8080 --bearer "$BLUE_ADMIN_TOKEN" --ttl 10m --note "cutover example"
cli cluster export --gateway http://blue-0:8080 --bearer "$(cat "$RUN/keys/mirror.key")" >"$RUN/state.json"
log "exported: $(jq -c '{frozen, stable: (.stable|length), deprecated: (.deprecated|length), keys: (.keys|length), externalDependents: (.externalDependents|length)}' "$RUN/state.json")"
cli cluster import --gateway http://green-0:8080 --bearer "$GREEN_ADMIN_TOKEN" --dry-run /work/state.json
cli cluster import --gateway http://green-0:8080 --bearer "$GREEN_ADMIN_TOKEN" /work/state.json
cli cluster unfreeze --gateway http://blue-0:8080 --bearer "$BLUE_ADMIN_TOKEN"
cli cluster export --gateway http://green-0:8080 --bearer "$GREEN_ADMIN_TOKEN" --allow-unfrozen 2>/dev/null >"$RUN/green-state.json" # for the comparison only; its unfrozen warning is expected
for k in stable deprecated externalDependents; do
	diff <(jq -S ".$k" "$RUN/state.json") <(jq -S ".$k" "$RUN/green-state.json") >/dev/null || die "green's $k differs from blue's"
done
# Green also holds its own system key (minted in step 1).
MISSING=$(jq -n --slurpfile b "$RUN/state.json" --slurpfile g "$RUN/green-state.json" '[$b[0].keys[] | select(. as $k | $g[0].keys | index([$k]) | not)] | length')
((MISSING == 0)) || die "$MISSING of blue's keys are not on green"
log "green equals blue: stable, deprecated, externalDependents equal; all $(jq '.keys|length' "$RUN/state.json") of blue's keys on green"
step_end

# --------------------------------------------------- 3. event stream mirror
step_begin "3. event stream mirror blue -> green"
cli cluster mirror --gateway http://green-0:8080 --bearer "$GREEN_ADMIN_TOKEN"
mirror_view() { admin green-0 GET /cluster/mirror | jq -c '.streams[] | select(.stream == "EV_orders_v1_OrderShipped")'; }
for _ in $(seq 1 60); do
	V=$(mirror_view)
	[[ $(jq .mirrored <<<"$V") == true && $(jq .lag <<<"$V") == 0 && $(jq .greenMsgs <<<"$V") -gt 0 ]] && break
	sleep 0.5
done
log "mirror: $(mirror_view)"
[[ $(jq .lag <<<"$V") == 0 ]] || die "mirror lag not 0"
step_end

# ------------------------------------------------------------ 4. switchover
step_begin "4. switchover 5 -> 50 -> 100"
show_switchover() {
	log "  blue switchover_total by outcome: $(for o in green fallback error blue; do printf '%s=%s ' $o "$(blue_metric "switchover_total\\{outcome=\"$o\"\\}")"; done)green_errors=$(blue_metric 'switchover_green_errors_total')"
	log "  traffic by Envoy cluster: $(traffic | jq -c .byCluster)"
}
for p in 5 50 100; do
	cli cluster switchover --gateway http://blue-0:8080 --bearer "$BLUE_ADMIN_TOKEN" --percent $p >/dev/null
	sleep 8
	log "at $p%:"
	show_switchover
done
SW_GREEN=$(blue_metric 'switchover_total\{outcome="green"\}')
SW_BAD=$(echo "$(blue_metric 'switchover_total\{outcome="fallback"\}') + $(blue_metric 'switchover_total\{outcome="error"\}') + $(blue_metric 'switchover_green_errors_total')" | bc)
step_end

# ------------------------------------------- 5. external switch at Envoy
step_begin "5. external switch: Envoy routes to green"
envoy_blue() { curl -s 'http://127.0.0.1:28001/stats?filter=^cluster\.blue\.upstream_rq_(active|total)$' | tr '\n' ' '; }
log "envoy before: $(envoy_blue)"
curl -sf -X POST 'http://127.0.0.1:28001/runtime_modify?routing.gw.blue=0&routing.gw.green=100' >/dev/null
log "runtime routing.gw.blue=0 routing.gw.green=100"
# Blue is idle when Envoy has no active request to it and its request
# count stops moving.
prev=""
for _ in $(seq 1 40); do
	cur=$(envoy_blue)
	if [[ "$cur" == "$prev" && "$cur" == *"upstream_rq_active: 0"* ]]; then break; fi
	prev=$cur
	sleep 1
done
log "envoy after:  $cur"
[[ "$cur" == "$prev" ]] || die "blue still gets requests from Envoy"
B0=$(traffic | jq '.byCluster.blue // 0')
sleep 3
B1=$(traffic | jq '.byCluster.blue // 0')
((B1 == B0)) || die "clients still answered by blue"
log "no client request reaches blue: $(traffic | jq -c .byCluster)"
step_end

# --------------------------------- 6. events: consumers and producers move
step_begin "6. events: stop producer, drain, stop mirror, move consumer, move producer"
app /producer/stop POST >/dev/null
for _ in $(seq 1 60); do
	V=$(mirror_view)
	S=$(app /status)
	[[ $(jq .lag <<<"$V") == 0 && $(jq .greenMsgs <<<"$V") == $(jq .producedTotal <<<"$S") && $(jq .pending <<<"$S") == 0 ]] && break
	sleep 0.5
done
log "producer stopped: produced=$(jq -c .produced <<<"$S") blue consumer pending=$(jq .pending <<<"$S"); mirror $(jq -c '{blueMsgs, greenMsgs, lag}' <<<"$V")"
[[ $(jq .pending <<<"$S") == 0 ]] || die "blue consumer still has pending messages"
BLUE_MSGS=$(jq .blueMsgs <<<"$V")
cli cluster mirror --gateway http://green-0:8080 --bearer "$GREEN_ADMIN_TOKEN" --stop
app '/consumer/move?to=green' POST >/dev/null
app '/producer/start?on=green' POST >/dev/null
sleep 3
log "consumer and producer on green: $(app /status | jq -c '{producerOn, consumerOn, produced, consumed}')"
step_end

# ------------------------------------------------------ 7. drain blue
step_begin "7. drain and stop blue (SIGTERM, --shutdown-delay 5s)"
app '/deregister?cluster=blue' POST >/dev/null
log "app deregistered from blue"
probe_log="$RUN/blue-0-health.log"
(
	last=""
	while :; do
		r=$(curl -s -m 1 -w ' %{http_code}' "$(url_of blue-0)/api/health" 2>/dev/null || true)
		code=${r##* }
		st=$(jq -r .status <<<"${r% *}" 2>/dev/null || true)
		[[ $code == 000 ]] && st="connection refused"
		[[ "$code $st" != "$last" ]] && printf '[%ss] blue-0 /api/health: %s %s\n' "$(el)" "$code" "$st"
		last="$code $st"
		[[ $code == 000 ]] && break
		sleep 0.2
	done
) >"$probe_log" &
PROBE=$!
D0=$(date +%s.%N)
dcq stop -t 90 "${blue_nodes[@]}"
log "blue stopped in $(printf '%.1f' "$(echo "$(date +%s.%N) - $D0" | bc)") s"
wait $PROBE || true
cat "$probe_log"
grep -q draining "$probe_log" || die "blue-0 never reported draining"
step_end

# ---------------------------------------------------------------- finish
step_begin "8. green alone"
sleep 5
app /producer/stop POST >/dev/null
for _ in $(seq 1 60); do
	S=$(app '/status?streams=green')
	[[ $(jq .pending <<<"$S") == 0 && $(jq .unique <<<"$S") == $(jq .producedTotal <<<"$S") ]] && break
	sleep 0.5
done
step_end
curl -sf -X POST http://127.0.0.1:28100/stop >/dev/null
FINAL=$(traffic)

echo
log "=== summary"
printf '%-72s %8s %9s %7s\n' STEP SECONDS REQUESTS FAILED
for i in "${!STEP_NAMES[@]}"; do
	printf '%-72s %8s %9s %7s\n' "${STEP_NAMES[$i]}" "${STEP_SECS[$i]}" "${STEP_REQS[$i]}" "${STEP_FAILS[$i]}"
done
log "traffic: $(jq -c '{total, ok, failed, byOp, byCluster}' <<<"$FINAL")"
log "events: $(jq -c '{producedTotal, produced, consumed, unique, duplicates, missing, greenStreamMsgs: .streamMsgs.green}' <<<"$S")"
log "events on blue's stream at the mirror stop: $BLUE_MSGS"
log "switchover: forwarded to green=$SW_GREEN, fallback+error+green_errors=$SW_BAD"

ok=1
FAILED=$(jq .failed <<<"$FINAL")
if ((FAILED != 0)); then
	ok=0
	log "FAIL: $FAILED client requests failed"
	jq -r '.lastFailures[] | "  \(.at) \(.client) \(.op): \(.error)"' <<<"$FINAL"
fi
[[ $(jq .missing <<<"$S") == 0 && $(jq .duplicates <<<"$S") == 0 && $(jq .unique <<<"$S") == $(jq .producedTotal <<<"$S") ]] || {
	ok=0
	log "FAIL: events lost or duplicated"
}
[[ $(jq .streamMsgs.green <<<"$S") == $(jq .producedTotal <<<"$S") ]] || {
	ok=0
	log "FAIL: green's stream does not hold every event"
}
[[ $(jq '.produced.blue' <<<"$S") == "$BLUE_MSGS" ]] || {
	ok=0
	log "FAIL: blue's stream count differs from what was produced on blue"
}
if ((SW_GREEN == 0)) || [[ $SW_BAD != 0 ]]; then
	ok=0
	log "FAIL: switchover metrics"
fi
((ok == 1)) || exit 1
log "PASS: 0 failed client requests, state equal, no event lost or duplicated, switchover clean"
