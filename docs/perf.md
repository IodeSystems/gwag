<!--
DO NOT EDIT — this file regenerates from sweep JSONs via 'bin/bench perf report'.
Run 'bin/bench perf all' to refresh the inputs under bench/.run/perf/.
-->

# Performance

> _Generated 2026-11-07T03:12:45Z from 3 scenario sweeps via `bin/bench perf report`._

**Headline (proto scenario, last healthy rung):** **7318 RPS** at p95 **11.33ms** with gateway self-time mean **32µs**.

> **Looking for "how does gwag compare to X?"** This page is gwag's
> own throughput on your hardware. For a head-to-head against
> graphql-mesh, Apollo Router and gat on the same backends, see
> [`compare/comparison.md`](../compare/comparison.md); for
> gat against gqlgen / connect-go / grpc-gateway in-process, see
> [`compare/gatbench/`](../compare/gatbench/README.md).

## Machine

| Field | Value |
|---|---|
| Captured at | 2026-11-07T03:10:35Z |
| CPU | Neoverse-V1 |
| Cores (logical) | 2 |
| RAM | 7.6 GiB |
| OS | Ubuntu 24.04 |
| Kernel | 7.0.0-1012-aws |
| Arch | arm64 |
| Go | go1.27.3 |
| Gateway rev | 84e7377 (dirty) |


## Scenario: `graphql`

- Endpoint: `http://localhost:18080/api/graphql`
- Duration per rep: `5.0s` × 3 reps (rep 1 discarded as warm-up)

| Target RPS | Achieved | Client mean | p50 | p95 | p99 | Gateway self (mean) | Dispatch (mean) |
|---:|---:|---:|---:|---:|---:|---:|---:|
| 250 | 250 | 572µs | 562µs | 640µs | 847µs | 59µs | 287µs |
| 500 | 500 | 514µs | 501µs | 576µs | 843µs | 51µs | 261µs |
| 1000 | 999 | 605µs | 573µs | 725µs | 1.43ms | 51µs | 301µs |
| 2000 | 1990 | 871µs | 727µs | 1.36ms | 4.54ms | 38µs | 421µs |
| 3000 | 2989 | 1.23ms | 892µs | 2.52ms | 11.81ms | 36µs | 592µs |
| 5000 | 4926 | 2.33ms | 1.38ms | 7.90ms | 14.77ms | 35µs | 1.14ms |
| 7500 | 7030 | 12.45ms | 5.93ms | 42.91ms | 59.42ms | 113µs | 6.17ms |

**Knee detected at 7500 RPS** (latency_above_50ms): p99 59421µs (59.4ms) exceeds 50ms SLA ceiling. Recommended ceiling: **5000 RPS** on this host.

### Interpretation

**~2463 RPS / core** across 2 logical cores at the recommended ceiling. Gateway self-time mean is **35µs** at the recommended ceiling — this is the per-request overhead the gateway adds on top of whatever the upstream takes.

### Where time + allocs go (5000 RPS, 20s CPU window)

**Top CPU (cumulative):**

```
File: gateway
Build ID: b6f59b5ed281231ea670cf7da3eb1f84bc306edd
Type: cpu
Time: 2026-11-07 03:12:23 UTC
Duration: 20s, Total samples = 14.93s (74.65%)
Showing nodes accounting for 9.67s, 64.77% of 14.93s total
Dropped 598 nodes (cum <= 0.07s)
      flat  flat%   sum%        cum   cum%
     0.02s  0.13%  0.13%      8.99s 60.21%  net/http.(*conn).serve
         0     0%  0.13%      5.20s 34.83%  net/http.(*ServeMux).ServeHTTP
         0     0%  0.13%      5.20s 34.83%  net/http.serverHandler.ServeHTTP
         0     0%  0.13%      5.16s 34.56%  github.com/iodesystems/gwag/v2/gw/gat.(*Gateway).Handler.func1
         0     0%  0.13%      5.16s 34.56%  net/http.HandlerFunc.ServeHTTP
     0.01s 0.067%   0.2%      4.91s 32.89%  github.com/iodesystems/gwag/v2/gw/gat.(*Gateway).serveRequest
     4.21s 28.20% 28.40%      4.21s 28.20%  internal/runtime/syscall/linux.Syscall6
         0     0% 28.40%      4.14s 27.73%  internal/poll.ignoringEINTRIO (inline)
         0     0% 28.40%      4.11s 27.53%  github.com/iodesystems/gwag/v2/gw/gat.(*Gateway).run
         0     0% 28.40%      4.11s 27.53%  syscall.Syscall
```

**Top allocs (cumulative alloc_space):**

```
File: gateway
Build ID: b6f59b5ed281231ea670cf7da3eb1f84bc306edd
Type: alloc_space
Time: 2026-11-07 03:12:43 UTC
Showing nodes accounting for 16981.71MB, 87.60% of 19385.24MB total
Dropped 634 nodes (cum <= 96.93MB)
      flat  flat%   sum%        cum   cum%
      30MB  0.15%  0.15% 17126.44MB 88.35%  net/http.(*conn).serve
         0     0%  0.15% 14714.33MB 75.90%  net/http.(*ServeMux).ServeHTTP
         0     0%  0.15% 14714.33MB 75.90%  net/http.HandlerFunc.ServeHTTP
         0     0%  0.15% 14714.33MB 75.90%  net/http.serverHandler.ServeHTTP
         0     0%  0.15% 14573.54MB 75.18%  github.com/iodesystems/gwag/v2/gw/gat.(*Gateway).Handler.func1
      79MB  0.41%  0.56% 13836.96MB 71.38%  github.com/iodesystems/gwag/v2/gw/gat.(*Gateway).serveRequest
         0     0%  0.56% 11346.59MB 58.53%  github.com/iodesystems/gwag/v2/gw/gat.(*Gateway).run
         0     0%  0.56% 11346.59MB 58.53%  github.com/iodesystems/gwag/v2/gw/gat.execute
         0     0%  0.56% 11288.87MB 58.23%  github.com/IodeSystems/graphql-go/v2.ExecutePlanAppend
         0     0%  0.56% 11288.87MB 58.23%  github.com/IodeSystems/graphql-go/v2.ExecutePlanAppend.func1
```

Raw pprof files: `profile-graphql.cpu.pprof` + `profile-graphql.allocs.pprof` under the sweep out-dir; inspect interactively with `go tool pprof`.

## Scenario: `openapi`
pure OpenAPI/HTTP backend (hello_openapi); same Hello shape via HTTP/JSON.


- Endpoint: `http://localhost:18080/api/graphql`
- Duration per rep: `5.0s` × 3 reps (rep 1 discarded as warm-up)

| Target RPS | Achieved | Client mean | p50 | p95 | p99 | Gateway self (mean) | Dispatch (mean) |
|---:|---:|---:|---:|---:|---:|---:|---:|
| 250 | 250 | 557µs | 543µs | 618µs | 859µs | 64µs | 265µs |
| 500 | 500 | 495µs | 473µs | 558µs | 1.02ms | 53µs | 237µs |
| 1000 | 999 | 556µs | 535µs | 679µs | 1.23ms | 55µs | 255µs |
| 2000 | 1998 | 725µs | 673µs | 966µs | 2.73ms | 41µs | 322µs |
| 3000 | 2996 | 977µs | 855µs | 1.81ms | 3.93ms | 38µs | 442µs |
| 5000 | 4827 | 3.99ms | 1.38ms | 18.16ms | 33.84ms | 40µs | 1.95ms |
| 7500 | 7139 | 8.31ms | 3.38ms | 30.00ms | 44.94ms | 62µs | 4.07ms |
| 10000 | 8809 | 53.87ms | 54.55ms | 87.32ms | 105.95ms | 3.61ms | 24.88ms |

**Knee detected at 10000 RPS** (latency_above_50ms): p99 105949µs (105.9ms) exceeds 50ms SLA ceiling. Recommended ceiling: **7500 RPS** on this host.

### Interpretation

**~3569 RPS / core** across 2 logical cores at the recommended ceiling. Gateway self-time mean is **62µs** at the recommended ceiling — this is the per-request overhead the gateway adds on top of whatever the upstream takes.

### Where time + allocs go (7500 RPS, 20s CPU window)

**Top CPU (cumulative):**

```
File: gateway
Build ID: b6f59b5ed281231ea670cf7da3eb1f84bc306edd
Type: cpu
Time: 2026-11-07 03:10:13 UTC
Duration: 20s, Total samples = 20.43s (102.14%)
Showing nodes accounting for 13.06s, 63.93% of 20.43s total
Dropped 668 nodes (cum <= 0.10s)
      flat  flat%   sum%        cum   cum%
     0.03s  0.15%  0.15%     12.66s 61.97%  net/http.(*conn).serve
         0     0%  0.15%      7.82s 38.28%  net/http.(*ServeMux).ServeHTTP
         0     0%  0.15%      7.82s 38.28%  net/http.serverHandler.ServeHTTP
     0.01s 0.049%   0.2%      7.64s 37.40%  net/http.HandlerFunc.ServeHTTP
     0.03s  0.15%  0.34%      7.63s 37.35%  github.com/iodesystems/gwag/v2/gw/gat.(*Gateway).Handler.func1
     0.07s  0.34%  0.69%      7.24s 35.44%  github.com/iodesystems/gwag/v2/gw/gat.(*Gateway).serveRequest
         0     0%  0.69%      5.93s 29.03%  github.com/iodesystems/gwag/v2/gw/gat.(*Gateway).run
     0.03s  0.15%  0.83%      5.92s 28.98%  github.com/iodesystems/gwag/v2/gw/gat.execute
     0.01s 0.049%  0.88%      5.88s 28.78%  github.com/IodeSystems/graphql-go/v2.ExecutePlanAppend
         0     0%  0.88%      5.87s 28.73%  github.com/IodeSystems/graphql-go/v2.ExecutePlanAppend.func1
```

**Top allocs (cumulative alloc_space):**

```
File: gateway
Build ID: b6f59b5ed281231ea670cf7da3eb1f84bc306edd
Type: alloc_space
Time: 2026-11-07 03:10:33 UTC
Showing nodes accounting for 12655.17MB, 89.59% of 14124.94MB total
Dropped 571 nodes (cum <= 70.62MB)
      flat  flat%   sum%        cum   cum%
   21.50MB  0.15%  0.15% 12434.39MB 88.03%  net/http.(*conn).serve
         0     0%  0.15% 10646.44MB 75.37%  net/http.(*ServeMux).ServeHTTP
         0     0%  0.15% 10646.44MB 75.37%  net/http.HandlerFunc.ServeHTTP
         0     0%  0.15% 10646.44MB 75.37%  net/http.serverHandler.ServeHTTP
         0     0%  0.15%    10551MB 74.70%  github.com/iodesystems/gwag/v2/gw/gat.(*Gateway).Handler.func1
      58MB  0.41%  0.56%  9995.45MB 70.76%  github.com/iodesystems/gwag/v2/gw/gat.(*Gateway).serveRequest
         0     0%  0.56%  8143.81MB 57.66%  github.com/iodesystems/gwag/v2/gw/gat.(*Gateway).run
         0     0%  0.56%  8143.81MB 57.66%  github.com/iodesystems/gwag/v2/gw/gat.execute
         0     0%  0.56%  8099.14MB 57.34%  github.com/IodeSystems/graphql-go/v2.ExecutePlanAppend
         0     0%  0.56%  8099.14MB 57.34%  github.com/IodeSystems/graphql-go/v2.ExecutePlanAppend.func1
```

Raw pprof files: `profile-openapi.cpu.pprof` + `profile-openapi.allocs.pprof` under the sweep out-dir; inspect interactively with `go tool pprof`.

## Scenario: `proto`
pure proto/gRPC backend (greeter); baseline for native-format dispatch cost.


- Endpoint: `http://localhost:18080/api/graphql`
- Duration per rep: `5.0s` × 3 reps (rep 1 discarded as warm-up)

| Target RPS | Achieved | Client mean | p50 | p95 | p99 | Gateway self (mean) | Dispatch (mean) |
|---:|---:|---:|---:|---:|---:|---:|---:|
| 250 | 250 | 599µs | 585µs | 697µs | 951µs | 65µs | 298µs |
| 500 | 500 | 536µs | 522µs | 631µs | 937µs | 56µs | 267µs |
| 1000 | 999 | 615µs | 593µs | 771µs | 1.29ms | 54µs | 296µs |
| 2000 | 1995 | 821µs | 743µs | 1.32ms | 2.99ms | 37µs | 400µs |
| 3000 | 2994 | 1.06ms | 905µs | 1.94ms | 4.57ms | 35µs | 547µs |
| 5000 | 4968 | 1.84ms | 1.29ms | 4.74ms | 12.30ms | 32µs | 983µs |
| 7500 | 7318 | 3.54ms | 2.13ms | 11.33ms | 18.33ms | 32µs | 1.98ms |
| 10000 | 9292 | 11.07ms | 6.43ms | 30.82ms | 74.85ms | 383µs | 6.25ms |

**Knee detected at 10000 RPS** (latency_above_50ms): p99 74848µs (74.8ms) exceeds 50ms SLA ceiling. Recommended ceiling: **7500 RPS** on this host.

### Interpretation

**~3659 RPS / core** across 2 logical cores at the recommended ceiling. Gateway self-time mean is **32µs** at the recommended ceiling — this is the per-request overhead the gateway adds on top of whatever the upstream takes.

### Where time + allocs go (7500 RPS, 20s CPU window)

**Top CPU (cumulative):**

```
File: gateway
Build ID: b6f59b5ed281231ea670cf7da3eb1f84bc306edd
Type: cpu
Time: 2026-11-07 03:07:47 UTC
Duration: 20s, Total samples = 18.63s (93.15%)
Showing nodes accounting for 11.72s, 62.91% of 18.63s total
Dropped 703 nodes (cum <= 0.09s)
      flat  flat%   sum%        cum   cum%
     0.02s  0.11%  0.11%     12.35s 66.29%  net/http.(*conn).serve
     0.01s 0.054%  0.16%      7.30s 39.18%  net/http.serverHandler.ServeHTTP
         0     0%  0.16%      7.29s 39.13%  net/http.(*ServeMux).ServeHTTP
     0.02s  0.11%  0.27%      7.21s 38.70%  github.com/iodesystems/gwag/v2/gw/gat.(*Gateway).Handler.func1
         0     0%  0.27%      7.21s 38.70%  net/http.HandlerFunc.ServeHTTP
     0.02s  0.11%  0.38%      6.79s 36.45%  github.com/iodesystems/gwag/v2/gw/gat.(*Gateway).serveRequest
     0.01s 0.054%  0.43%      5.63s 30.22%  github.com/iodesystems/gwag/v2/gw/gat.(*Gateway).run
     0.03s  0.16%  0.59%      5.62s 30.17%  github.com/iodesystems/gwag/v2/gw/gat.execute
         0     0%  0.59%      5.59s 30.01%  github.com/IodeSystems/graphql-go/v2.ExecutePlanAppend
     0.01s 0.054%  0.64%      5.59s 30.01%  github.com/IodeSystems/graphql-go/v2.ExecutePlanAppend.func1
```

**Top allocs (cumulative alloc_space):**

```
File: gateway
Build ID: b6f59b5ed281231ea670cf7da3eb1f84bc306edd
Type: alloc_space
Time: 2026-11-07 03:08:07 UTC
Showing nodes accounting for 6.20GB, 92.73% of 6.68GB total
Dropped 453 nodes (cum <= 0.03GB)
      flat  flat%   sum%        cum   cum%
    0.01GB  0.18%  0.18%     5.79GB 86.59%  net/http.(*conn).serve
         0     0%  0.18%     4.90GB 73.32%  net/http.(*ServeMux).ServeHTTP
         0     0%  0.18%     4.90GB 73.32%  net/http.HandlerFunc.ServeHTTP
         0     0%  0.18%     4.90GB 73.32%  net/http.serverHandler.ServeHTTP
         0     0%  0.18%     4.85GB 72.53%  github.com/iodesystems/gwag/v2/gw/gat.(*Gateway).Handler.func1
    0.03GB  0.42%   0.6%     4.58GB 68.59%  github.com/iodesystems/gwag/v2/gw/gat.(*Gateway).serveRequest
         0     0%   0.6%     3.66GB 54.82%  github.com/iodesystems/gwag/v2/gw/gat.(*Gateway).run
         0     0%   0.6%     3.66GB 54.82%  github.com/iodesystems/gwag/v2/gw/gat.execute
         0     0%   0.6%     3.64GB 54.52%  github.com/IodeSystems/graphql-go/v2.ExecutePlanAppend
         0     0%   0.6%     3.64GB 54.52%  github.com/IodeSystems/graphql-go/v2.ExecutePlanAppend.func1
```

Raw pprof files: `profile-proto.cpu.pprof` + `profile-proto.allocs.pprof` under the sweep out-dir; inspect interactively with `go tool pprof`.


## Tracing overhead

`WithTracer` is opt-in. When unset, the gateway wires a no-op
tracer and the per-request hot path stays branch-free. When set, the
gateway opens one server-kind span per ingress + one client-kind span
per dispatch, extracts inbound `traceparent`, and injects on
outbound HTTP / gRPC.

Microbench delta from `BenchmarkTracing_GraphQLIngress_*` —
GraphQL ingress over loopback gRPC, `-benchtime=3s -count=3`:

| Config | ns/op (range) | B/op | allocs/op |
|---|---|---|---|
| Tracing off (noop) | ~386k–424k | ~37.7 KB | 359 |
| Tracing on, sync exporter | ~373k–391k | ~44.4 KB | 380 |
| Tracing on, batching exporter | ~377k–382k | ~44.5 KB | 376 |

**+21 allocs and ~6.7 KB per request when tracing is enabled.** The
wall-time delta is below the HTTP-loopback noise floor on this host —
the sync exporter run overlaps the noop baseline. Sampling and
exporter wire time are separate operator concerns; use
`TraceIDRatioBased` for production volumes.

Reproduce:

```bash
go test ./gw/ -bench=BenchmarkTracing_GraphQLIngress -benchmem -run=^$ -benchtime=3s -count=3
```

Wiring + span reference: [`docs/tracing.md`](./tracing.md).

## How to read this

Three numbers tell most of the story per scenario:

- **Achieved RPS / target RPS** — anything < 80 % of target is saturation (gateway, client, or upstream).
- **Gateway self (mean)** — the gateway-only slice of each request (`request_self_seconds` mean). Compare across upstream-latency runs to see "how much does the gateway add"; this number should be roughly upstream-independent.
- **Dispatch (mean)** — upstream time as measured by the gateway. Climbs with configured upstream latency; the delta vs. self-time is the upstream's contribution.

### Knee heuristic

A rung is flagged as the knee when:

- **achieved_below_80pct** — actual RPS < 0.80 × target. The client / gateway / upstream couldn't keep up; throughput collapsed.
- **p99_cliff** — step's p99 > 2 × prior step's p99 **AND** achieved RPS no longer climbed vs the prior step. Catches saturation-via-latency where the gateway goes slow rather than dropping. A pure latency creep with healthy throughput growth is normal queueing under load, not a knee, and is intentionally not flagged.
- **latency_above_50ms** — step's p99 > 50ms. Absolute SLA ceiling — catches the case where throughput keeps climbing past the gateway's healthy zone but p99 has deteriorated past what any production deployment would tolerate.

First-firing predicate stops the sweep; the prior step is the recommended ceiling. Pass `--no-knee` to `bench perf run` to walk every rung regardless (useful for the full curve).

### Regenerating

The one-command path reads `bench/perf-scenarios.yaml`, brings up
the stack and the upstream services each scenario needs, runs every
sweep, and renders this file:

```bash
bin/bench perf
```

Customise the sweep (different RPS rungs, your own query, regression
runs) by editing `bench/perf-scenarios.yaml` or passing
`--config path/to/your.yaml`.

Subcommands for power users:

```bash
bin/bench perf specs                  # print host-specs header only
bin/bench perf run --scenario proto   # one ad-hoc sweep
bin/bench perf report --in-dir ...    # re-render without re-running
```
