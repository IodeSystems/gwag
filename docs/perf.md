<!--
DO NOT EDIT — this file regenerates from sweep JSONs via 'bin/bench perf report'.
Run 'bin/bench perf all' to refresh the inputs under bench/.run/perf/.
-->

# Performance

> _Generated 2026-05-25T23:12:21Z from 3 scenario sweeps via `bin/bench perf report`._

**Headline (proto scenario, last healthy rung):** **48713 RPS** at p95 **22.83ms** with gateway self-time mean **1.21ms**.

> **Looking for "how does gwag compare to X?"** This page is gwag's
> own throughput on your hardware. For a head-to-head against
> graphql-mesh and Apollo Router on the same backends, see
> [`perf/comparison.md`](../perf/comparison.md).

## Machine

| Field | Value |
|---|---|
| Captured at | 2026-05-25T23:10:10Z |
| CPU | AMD Ryzen 9 3900X 12-Core Processor |
| Cores (logical) | 24 |
| RAM | 125.7 GiB |
| OS | Ubuntu 24.04 |
| Kernel | 6.8.0-111-generic |
| Arch | amd64 |
| Go | go1.26.2 |
| Gateway rev | 9a2d840 |


## Scenario: `graphql`

- Endpoint: `http://localhost:18080/api/graphql`
- Duration per rep: `5.0s` × 3 reps (rep 1 discarded as warm-up)

| Target RPS | Achieved | Client mean | p50 | p95 | p99 | Gateway self (mean) | Dispatch (mean) |
|---:|---:|---:|---:|---:|---:|---:|---:|
| 1000 | 999 | 464µs | 460µs | 528µs | 710µs | 37µs | 228µs |
| 5000 | 4990 | 581µs | 553µs | 769µs | 1.26ms | 42µs | 291µs |
| 10000 | 9980 | 715µs | 644µs | 1.21ms | 2.23ms | 40µs | 347µs |
| 20000 | 19960 | 753µs | 665µs | 1.37ms | 2.95ms | 31µs | 313µs |
| 30000 | 29978 | 1.06ms | 904µs | 2.43ms | 3.80ms | 36µs | 359µs |
| 40000 | 39799 | 2.05ms | 1.37ms | 5.66ms | 13.35ms | 102µs | 668µs |
| 50000 | 46852 | 22.04ms | 19.04ms | 51.99ms | 69.47ms | 7.64ms | 3.27ms |

**Knee detected at 50000 RPS** (latency_above_50ms): p99 69468µs (69.5ms) exceeds 50ms SLA ceiling. Recommended ceiling: **40000 RPS** on this host.

### Interpretation

**~1658 RPS / core** across 24 logical cores at the recommended ceiling. Gateway self-time mean is **102µs** at the recommended ceiling — this is the per-request overhead the gateway adds on top of whatever the upstream takes.

### Where time + allocs go (40000 RPS, 20s CPU window)

**Top CPU (cumulative):**

```
File: gateway
Build ID: ab79d864ef37d0dc74ca8d131d31970083103757
Type: cpu
Time: 2026-05-25 16:11:59 PDT
Duration: 20s, Total samples = 156.80s (783.97%)
Showing nodes accounting for 97.12s, 61.94% of 156.80s total
Dropped 1098 nodes (cum <= 0.78s)
      flat  flat%   sum%        cum   cum%
     0.45s  0.29%  0.29%     88.80s 56.63%  net/http.(*conn).serve
     0.03s 0.019%  0.31%     54.13s 34.52%  net/http.serverHandler.ServeHTTP
     0.08s 0.051%  0.36%     54.10s 34.50%  net/http.(*ServeMux).ServeHTTP
     0.06s 0.038%   0.4%     53.16s 33.90%  net/http.HandlerFunc.ServeHTTP
     0.26s  0.17%  0.56%     53.08s 33.85%  github.com/iodesystems/gwag/gw.(*Gateway).Handler.func1
     0.28s  0.18%  0.74%     50.74s 32.36%  github.com/iodesystems/gwag/gw.(*Gateway).serveGraphQLJSON
     0.16s   0.1%  0.84%     42.70s 27.23%  github.com/IodeSystems/graphql-go.ExecutePlanAppend
         0     0%  0.84%     42.50s 27.10%  github.com/IodeSystems/graphql-go.ExecutePlanAppend.func1
     0.29s  0.18%  1.03%     42.50s 27.10%  github.com/IodeSystems/graphql-go.writePlannedSelection
     0.97s  0.62%  1.65%     42.28s 26.96%  github.com/IodeSystems/graphql-go.writePlannedField
```

**Top allocs (cumulative alloc_space):**

```
File: gateway
Build ID: ab79d864ef37d0dc74ca8d131d31970083103757
Type: alloc_space
Time: 2026-05-25 16:12:19 PDT
Showing nodes accounting for 230060.14MB, 89.79% of 256213.14MB total
Dropped 726 nodes (cum <= 1281.07MB)
      flat  flat%   sum%        cum   cum%
     315MB  0.12%  0.12% 225766.21MB 88.12%  net/http.(*conn).serve
         0     0%  0.12% 194151.25MB 75.78%  net/http.(*ServeMux).ServeHTTP
         0     0%  0.12% 194151.25MB 75.78%  net/http.HandlerFunc.ServeHTTP
         0     0%  0.12% 194151.25MB 75.78%  net/http.serverHandler.ServeHTTP
 3802.20MB  1.48%  1.61% 193449.87MB 75.50%  github.com/iodesystems/gwag/gw.(*Gateway).Handler.func1
         0     0%  1.61% 183640.91MB 71.68%  github.com/iodesystems/gwag/gw.(*Gateway).serveGraphQLJSON
         0     0%  1.61% 156936.58MB 61.25%  github.com/IodeSystems/graphql-go.ExecutePlanAppend
         0     0%  1.61% 156933.58MB 61.25%  github.com/IodeSystems/graphql-go.ExecutePlanAppend.func1
  920.04MB  0.36%  1.97% 156933.58MB 61.25%  github.com/IodeSystems/graphql-go.writePlannedSelection
 2820.70MB  1.10%  3.07% 156013.54MB 60.89%  github.com/IodeSystems/graphql-go.writePlannedField
```

Raw pprof files: `profile-graphql.cpu.pprof` + `profile-graphql.allocs.pprof` under the sweep out-dir; inspect interactively with `go tool pprof`.

## Scenario: `openapi`
pure OpenAPI/HTTP backend (hello_openapi); same Hello shape via HTTP/JSON.


- Endpoint: `http://localhost:18080/api/graphql`
- Duration per rep: `5.0s` × 3 reps (rep 1 discarded as warm-up)

| Target RPS | Achieved | Client mean | p50 | p95 | p99 | Gateway self (mean) | Dispatch (mean) |
|---:|---:|---:|---:|---:|---:|---:|---:|
| 1000 | 999 | 445µs | 444µs | 507µs | 625µs | 38µs | 209µs |
| 5000 | 4993 | 540µs | 521µs | 690µs | 1.04ms | 44µs | 252µs |
| 10000 | 9983 | 693µs | 640µs | 1.05ms | 2.14ms | 45µs | 304µs |
| 20000 | 19944 | 683µs | 619µs | 1.18ms | 2.61ms | 32µs | 257µs |
| 30000 | 29981 | 958µs | 847µs | 2.05ms | 3.19ms | 37µs | 295µs |
| 40000 | 39953 | 1.49ms | 1.21ms | 3.67ms | 5.52ms | 41µs | 408µs |
| 50000 | 48798 | 3.52ms | 2.39ms | 9.99ms | 15.08ms | 105µs | 867µs |
| 60000 | 56498 | 33.01ms | 44.69ms | 67.15ms | 107.46ms | 912µs | 1.72ms |

**Knee detected at 60000 RPS** (latency_above_50ms): p99 107464µs (107.5ms) exceeds 50ms SLA ceiling. Recommended ceiling: **50000 RPS** on this host.

### Interpretation

**~2033 RPS / core** across 24 logical cores at the recommended ceiling. Gateway self-time mean is **105µs** at the recommended ceiling — this is the per-request overhead the gateway adds on top of whatever the upstream takes.

### Where time + allocs go (50000 RPS, 20s CPU window)

**Top CPU (cumulative):**

```
File: gateway
Build ID: ab79d864ef37d0dc74ca8d131d31970083103757
Type: cpu
Time: 2026-05-25 16:09:48 PDT
Duration: 20s, Total samples = 183.19s (915.92%)
Showing nodes accounting for 115.96s, 63.30% of 183.19s total
Dropped 1107 nodes (cum <= 0.92s)
      flat  flat%   sum%        cum   cum%
     0.45s  0.25%  0.25%    103.55s 56.53%  net/http.(*conn).serve
     0.07s 0.038%  0.28%     60.18s 32.85%  net/http.serverHandler.ServeHTTP
     0.07s 0.038%  0.32%     60.11s 32.81%  net/http.(*ServeMux).ServeHTTP
     0.05s 0.027%  0.35%     59.21s 32.32%  net/http.HandlerFunc.ServeHTTP
     0.39s  0.21%  0.56%     59.16s 32.29%  github.com/iodesystems/gwag/gw.(*Gateway).Handler.func1
     0.40s  0.22%  0.78%     56.41s 30.79%  github.com/iodesystems/gwag/gw.(*Gateway).serveGraphQLJSON
     0.18s 0.098%  0.88%     46.98s 25.65%  github.com/IodeSystems/graphql-go.ExecutePlanAppend
     0.03s 0.016%   0.9%     46.79s 25.54%  github.com/IodeSystems/graphql-go.ExecutePlanAppend.func1
     0.46s  0.25%  1.15%     46.73s 25.51%  github.com/IodeSystems/graphql-go.writePlannedSelection
     1.42s  0.78%  1.92%     46.36s 25.31%  github.com/IodeSystems/graphql-go.writePlannedField
```

**Top allocs (cumulative alloc_space):**

```
File: gateway
Build ID: ab79d864ef37d0dc74ca8d131d31970083103757
Type: alloc_space
Time: 2026-05-25 16:10:08 PDT
Showing nodes accounting for 188977.76MB, 88.97% of 212408.10MB total
Dropped 724 nodes (cum <= 1062.04MB)
      flat  flat%   sum%        cum   cum%
  266.50MB  0.13%  0.13% 186476.78MB 87.79%  net/http.(*conn).serve
         0     0%  0.13% 159945.76MB 75.30%  net/http.(*ServeMux).ServeHTTP
         0     0%  0.13% 159945.76MB 75.30%  net/http.HandlerFunc.ServeHTTP
         0     0%  0.13% 159945.76MB 75.30%  net/http.serverHandler.ServeHTTP
 3181.58MB  1.50%  1.62% 159301.95MB 75.00%  github.com/iodesystems/gwag/gw.(*Gateway).Handler.func1
         0     0%  1.62% 151089.14MB 71.13%  github.com/iodesystems/gwag/gw.(*Gateway).serveGraphQLJSON
         0     0%  1.62% 128735.71MB 60.61%  github.com/IodeSystems/graphql-go.ExecutePlanAppend
         0     0%  1.62% 128733.71MB 60.61%  github.com/IodeSystems/graphql-go.ExecutePlanAppend.func1
  779.54MB  0.37%  1.99% 128733.71MB 60.61%  github.com/IodeSystems/graphql-go.writePlannedSelection
 2260.16MB  1.06%  3.05% 127954.18MB 60.24%  github.com/IodeSystems/graphql-go.writePlannedField
```

Raw pprof files: `profile-openapi.cpu.pprof` + `profile-openapi.allocs.pprof` under the sweep out-dir; inspect interactively with `go tool pprof`.

## Scenario: `proto`
pure proto/gRPC backend (greeter); baseline for native-format dispatch cost.


- Endpoint: `http://localhost:18080/api/graphql`
- Duration per rep: `5.0s` × 3 reps (rep 1 discarded as warm-up)

| Target RPS | Achieved | Client mean | p50 | p95 | p99 | Gateway self (mean) | Dispatch (mean) |
|---:|---:|---:|---:|---:|---:|---:|---:|
| 1000 | 998 | 511µs | 509µs | 595µs | 814µs | 38µs | 256µs |
| 5000 | 4996 | 679µs | 634µs | 1.09ms | 1.39ms | 41µs | 339µs |
| 10000 | 9983 | 836µs | 738µs | 1.55ms | 2.10ms | 39µs | 380µs |
| 20000 | 19981 | 823µs | 730µs | 1.65ms | 2.36ms | 29µs | 348µs |
| 30000 | 29973 | 1.04ms | 924µs | 2.16ms | 3.41ms | 29µs | 416µs |
| 40000 | 39904 | 1.75ms | 1.38ms | 4.30ms | 7.00ms | 35µs | 643µs |
| 50000 | 48714 | 6.36ms | 3.11ms | 22.83ms | 39.37ms | 1.21ms | 1.43ms |
| 60000 | 57619 | 33.89ms | 46.44ms | 67.38ms | 107.30ms | 1.68ms | 2.15ms |

**Knee detected at 60000 RPS** (latency_above_50ms): p99 107297µs (107.3ms) exceeds 50ms SLA ceiling. Recommended ceiling: **50000 RPS** on this host.

### Interpretation

**~2030 RPS / core** across 24 logical cores at the recommended ceiling. Gateway self-time mean is **1.21ms** at the recommended ceiling — this is the per-request overhead the gateway adds on top of whatever the upstream takes.

### Where time + allocs go (50000 RPS, 20s CPU window)

**Top CPU (cumulative):**

```
File: gateway
Build ID: ab79d864ef37d0dc74ca8d131d31970083103757
Type: cpu
Time: 2026-05-25 16:07:22 PDT
Duration: 20s, Total samples = 166.76s (833.75%)
Showing nodes accounting for 104.51s, 62.67% of 166.76s total
Dropped 1208 nodes (cum <= 0.83s)
      flat  flat%   sum%        cum   cum%
     0.48s  0.29%  0.29%    107.41s 64.41%  net/http.(*conn).serve
     0.09s 0.054%  0.34%     63.19s 37.89%  net/http.serverHandler.ServeHTTP
     0.04s 0.024%  0.37%     63.10s 37.84%  net/http.(*ServeMux).ServeHTTP
     0.02s 0.012%  0.38%     62.03s 37.20%  net/http.HandlerFunc.ServeHTTP
     0.40s  0.24%  0.62%     62.01s 37.19%  github.com/iodesystems/gwag/gw.(*Gateway).Handler.func1
     0.31s  0.19%   0.8%     59.01s 35.39%  github.com/iodesystems/gwag/gw.(*Gateway).serveGraphQLJSON
     0.14s 0.084%  0.89%     50.42s 30.24%  github.com/IodeSystems/graphql-go.ExecutePlanAppend
     0.05s  0.03%  0.92%     50.27s 30.15%  github.com/IodeSystems/graphql-go.ExecutePlanAppend.func1
     0.40s  0.24%  1.16%     50.20s 30.10%  github.com/IodeSystems/graphql-go.writePlannedSelection
     0.85s  0.51%  1.67%     49.92s 29.94%  github.com/IodeSystems/graphql-go.writePlannedField
```

**Top allocs (cumulative alloc_space):**

```
File: gateway
Build ID: ab79d864ef37d0dc74ca8d131d31970083103757
Type: alloc_space
Time: 2026-05-25 16:07:42 PDT
Showing nodes accounting for 141214.04MB, 89.12% of 158449.17MB total
Dropped 711 nodes (cum <= 792.25MB)
      flat  flat%   sum%        cum   cum%
     201MB  0.13%  0.13% 138291.03MB 87.28%  net/http.(*conn).serve
         0     0%  0.13% 118413.32MB 74.73%  net/http.(*ServeMux).ServeHTTP
         0     0%  0.13% 118413.32MB 74.73%  net/http.HandlerFunc.ServeHTTP
         0     0%  0.13% 118413.32MB 74.73%  net/http.serverHandler.ServeHTTP
 2373.43MB  1.50%  1.62% 117841.79MB 74.37%  github.com/iodesystems/gwag/gw.(*Gateway).Handler.func1
         0     0%  1.62% 111704.69MB 70.50%  github.com/iodesystems/gwag/gw.(*Gateway).serveGraphQLJSON
         0     0%  1.62% 95054.10MB 59.99%  github.com/IodeSystems/graphql-go.ExecutePlanAppend
         0     0%  1.62% 95052.10MB 59.99%  github.com/IodeSystems/graphql-go.ExecutePlanAppend.func1
  584.53MB  0.37%  1.99% 95052.10MB 59.99%  github.com/IodeSystems/graphql-go.writePlannedSelection
 1573.61MB  0.99%  2.99% 94467.57MB 59.62%  github.com/IodeSystems/graphql-go.writePlannedField
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
