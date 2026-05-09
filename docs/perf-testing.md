# Running a perf sample

Recipe for capturing a CPU + alloc profile from the gateway under
synthetic load, plus the diff workflow for "did this change regress
anything?". Targets the local bench stack; no extra tooling beyond
the standard Go toolchain and the bench scripts in this repo.

## 1. Boot the stack with pprof enabled

The default `bin/bench up` does **not** expose `/debug/pprof/`
(pprof leaks goroutine + heap state; never make it public). For a
profile run, restart `n1` with `--pprof` directly:

```bash
bin/bench up                          # if not already running
source bench/.run/gateways/n1.env     # picks up HTTP_PORT, NATS_*, DATA
kill -TERM "$PID"                     # stop the bench-spawned n1
sleep 2

nohup bench/.run/bin/gateway \
  --node-name "$NAME" \
  --http ":$HTTP_PORT" --control-plane ":$CP_PORT" \
  --nats-listen ":$NATS_LISTEN" --nats-cluster ":$NATS_CLUSTER" \
  --nats-data "$DATA" \
  --pprof \
  > "bench/.run/logs/${NAME}.log" 2>&1 &
sed -i "s/^PID=.*/PID=$!/" "bench/.run/gateways/$NAME.env"
```

Pprof endpoints (admin-bearer-gated) at `/debug/pprof/{profile,heap,allocs,goroutine,...}`.

## 2. Run traffic and capture profiles in parallel

The 30-second profile window should overlap a steady-state load
phase — not the warm-up. Start the bench at least 3 s before the
profile capture.

```bash
TOKEN=$(cat bench/.run/nats/n1/admin-token)
mkdir -p bench/.run/profiles

bin/bench traffic graphql \
  --target http://localhost:18080/api/graphql \
  --rps 30000 --duration 35s \
  > /tmp/bench.out &

sleep 3
curl -s -H "Authorization: Bearer $TOKEN" \
  "http://localhost:18080/debug/pprof/profile?seconds=30" \
  -o bench/.run/profiles/cpu.pprof
curl -s -H "Authorization: Bearer $TOKEN" \
  "http://localhost:18080/debug/pprof/allocs" \
  -o bench/.run/profiles/allocs.pprof

wait
cat /tmp/bench.out
```

The `traffic` summary already prints client RPS, P50/P95/P99,
gateway-side per-backend dispatch, and a per-ingress request-time
row pulling `request_duration_seconds` + `request_self_seconds`. If
`concurrency × p50 < target rps` the runner appends a Little's-law
advisor; act on it before trusting the numbers.

## 3. Read the profile

```bash
go tool pprof -top -cum bench/.run/profiles/cpu.pprof | head -25
go tool pprof -top -sample_index=alloc_space bench/.run/profiles/allocs.pprof | head -20
go tool pprof -peek 'context.WithValue' bench/.run/profiles/allocs.pprof
go tool pprof -list 'someFunctionName' bench/.run/profiles/cpu.pprof
```

What each metric means:

| Output | Source | Best for |
|---|---|---|
| Client RPS / P50–P99 | bench traffic summary | end-to-end perception |
| `request_duration_seconds` | gateway request_metrics | total per-request gateway wall time |
| `request_self_seconds` | gateway request_metrics | gateway-only slice (total minus dispatch accumulator) |
| `dispatch_duration_seconds` | gateway per-backend | upstream slice; bucket-coarse for sub-millisecond |
| pprof CPU profile | `/debug/pprof/profile` | where time goes |
| pprof alloc profile | `/debug/pprof/allocs` | where allocations come from |

For sub-millisecond requests trust mean (sum/count) over the
histogram p-quantiles — the default Prometheus buckets start at
5 ms, so p50 reads as `2.5ms` for anything that fits the first
bucket regardless of true latency.

## 4. Compare two runs (the regression workflow)

Capture before and after, then diff:

```bash
# before change
mv bench/.run/profiles/cpu.pprof bench/.run/profiles/cpu-before.pprof
mv bench/.run/profiles/allocs.pprof bench/.run/profiles/allocs-before.pprof
# … apply change, rebuild, restart n1, re-run step 2 …
go tool pprof -top -base bench/.run/profiles/cpu-before.pprof bench/.run/profiles/cpu.pprof
go tool pprof -top -base bench/.run/profiles/allocs-before.pprof bench/.run/profiles/allocs.pprof
```

For the bench summary, run identical flags on both sides
(`--rps`, `--duration`, `--concurrency`). RPS, p50, p95, p99 should
move within noise; the per-ingress self-time row is the most
sensitive single number for gw-side regressions — a p95 jump there
without a corresponding dispatch jump points the finger inward.

## 5. Tear down

```bash
bin/bench down              # purges .run/ by default
bin/bench down --no-purge   # keep cached binaries + profiles
```

## Gotchas

- **Bench client cap.** Default `--concurrency 0 = auto = max(64, rps/20)` scales with the requested RPS; if you pin a low value, expect saturation drops and a slower-than-real headline RPS. The runner's advisor flags this at end-of-run.
- **Demo concurrency caps.** The example greeter's `--max-concurrency` / `--max-concurrency-per-instance` default to `0` (unbounded). If you bump them to demo backpressure, the gateway will dispatch within those caps regardless of its own state — don't read that as a gateway bottleneck.
- **gw on pprof leaks state.** Restart `n1` without `--pprof` after profiling (or use `bin/bench restart`) — admin bearer is the only gate.
- **graphql-go fork dominates.** Recent profiles show `ExecutePlan` / `resolvePlannedField` taking ~50 % of allocs and a quarter of CPU; further wins from gw-owned code are diminishing without touching the fork.
