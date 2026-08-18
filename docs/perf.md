<!--
DO NOT EDIT — this file regenerates from sweep JSONs via 'bin/bench perf report'.
Run 'bin/bench perf all' to refresh the inputs under bench/.run/perf/.
-->

# Performance

> _Generated 2026-08-18T01:49:00Z from 3 scenario sweeps via `bin/bench perf report`._

**Headline (proto scenario, last healthy rung):** **49583 RPS** at p95 **18.92ms** with gateway self-time mean **176µs**.

> **Looking for "how does gwag compare to X?"** This page is gwag's
> own throughput on your hardware. For a head-to-head against
> graphql-mesh, Apollo Router and gat on the same backends, see
> [`compare/comparison.md`](../compare/comparison.md); for
> gat against gqlgen / connect-go / grpc-gateway in-process, see
> [`compare/gatbench/`](../compare/gatbench/README.md).

## Machine

| Field | Value |
|---|---|
| Captured at | 2026-08-18T01:46:49Z |
| CPU | AMD Ryzen 9 3900X 12-Core Processor |
| Cores (logical) | 24 |
| RAM | 125.7 GiB |
| OS | Ubuntu 24.04 |
| Kernel | 6.8.0-137-generic |
| Arch | amd64 |
| Go | go1.26.2 |
| Gateway rev | 3dcce21 (dirty) |


## Scenario: `graphql`

- Endpoint: `http://localhost:18080/api/graphql`
- Duration per rep: `5.0s` × 3 reps (rep 1 discarded as warm-up)

| Target RPS | Achieved | Client mean | p50 | p95 | p99 | Gateway self (mean) | Dispatch (mean) |
|---:|---:|---:|---:|---:|---:|---:|---:|
| 1000 | 999 | 459µs | 462µs | 529µs | 659µs | 36µs | 227µs |
| 5000 | 4992 | 559µs | 535µs | 771µs | 1.11ms | 40µs | 284µs |
| 10000 | 9981 | 767µs | 686µs | 1.30ms | 2.38ms | 41µs | 389µs |
| 20000 | 19969 | 770µs | 680µs | 1.40ms | 2.78ms | 30µs | 339µs |
| 30000 | 29971 | 1.06ms | 897µs | 2.42ms | 3.92ms | 36µs | 373µs |
| 40000 | 39902 | 1.86ms | 1.35ms | 5.04ms | 8.94ms | 53µs | 606µs |
| 50000 | 47903 | 14.99ms | 9.74ms | 43.68ms | 60.57ms | 1.38ms | 2.20ms |

**Knee detected at 50000 RPS** (latency_above_50ms): p99 60572µs (60.6ms) exceeds 50ms SLA ceiling. Recommended ceiling: **40000 RPS** on this host.

### Interpretation

**~1663 RPS / core** across 24 logical cores at the recommended ceiling. Gateway self-time mean is **53µs** at the recommended ceiling — this is the per-request overhead the gateway adds on top of whatever the upstream takes.

### Where time + allocs go (40000 RPS, 20s CPU window)

**Top CPU (cumulative):**

```
File: gateway
Build ID: 54b2c8c4f700c66cdb23e4d0f7323b75a00d3de0
Type: cpu
Time: 2026-08-17 18:48:37 PDT
Duration: 20s, Total samples = 160.74s (803.65%)
Showing nodes accounting for 103.80s, 64.58% of 160.74s total
Dropped 1105 nodes (cum <= 0.80s)
      flat  flat%   sum%        cum   cum%
     0.28s  0.17%  0.17%     87.63s 54.52%  net/http.(*conn).serve
     0.01s 0.0062%  0.18%     50.55s 31.45%  net/http.serverHandler.ServeHTTP
     0.10s 0.062%  0.24%     50.54s 31.44%  net/http.(*ServeMux).ServeHTTP
     0.06s 0.037%  0.28%     49.63s 30.88%  net/http.HandlerFunc.ServeHTTP
     0.27s  0.17%  0.45%     49.57s 30.84%  github.com/iodesystems/gwag/gw.(*Gateway).Handler.func1
     0.27s  0.17%  0.62%     47.06s 29.28%  github.com/iodesystems/gwag/gw.(*Gateway).serveGraphQLJSON
     0.18s  0.11%  0.73%     40.98s 25.49%  github.com/IodeSystems/graphql-go.ExecutePlanAppend
     0.02s 0.012%  0.74%     40.79s 25.38%  github.com/IodeSystems/graphql-go.ExecutePlanAppend.func1
     0.29s  0.18%  0.92%     40.75s 25.35%  github.com/IodeSystems/graphql-go.writePlannedSelection
     0.98s  0.61%  1.53%     40.49s 25.19%  github.com/IodeSystems/graphql-go.writePlannedField
```

**Top allocs (cumulative alloc_space):**

```
File: gateway
Build ID: 54b2c8c4f700c66cdb23e4d0f7323b75a00d3de0
Type: alloc_space
Time: 2026-08-17 18:48:57 PDT
Showing nodes accounting for 133813.68MB, 89.31% of 149828.62MB total
Dropped 629 nodes (cum <= 749.14MB)
      flat  flat%   sum%        cum   cum%
  188.50MB  0.13%  0.13% 131954.37MB 88.07%  net/http.(*conn).serve
         0     0%  0.13% 113603.31MB 75.82%  net/http.(*ServeMux).ServeHTTP
         0     0%  0.13% 113603.31MB 75.82%  net/http.HandlerFunc.ServeHTTP
         0     0%  0.13% 113603.31MB 75.82%  net/http.serverHandler.ServeHTTP
 2126.89MB  1.42%  1.55% 113416.51MB 75.70%  github.com/iodesystems/gwag/gw.(*Gateway).Handler.func1
         0     0%  1.55% 107854.97MB 71.99%  github.com/iodesystems/gwag/gw.(*Gateway).serveGraphQLJSON
         0     0%  1.55% 92252.13MB 61.57%  github.com/IodeSystems/graphql-go.ExecutePlanAppend
         0     0%  1.55% 92250.63MB 61.57%  github.com/IodeSystems/graphql-go.ExecutePlanAppend.func1
  564.03MB  0.38%  1.92% 92250.63MB 61.57%  github.com/IodeSystems/graphql-go.writePlannedSelection
 1509.11MB  1.01%  2.93% 91686.60MB 61.19%  github.com/IodeSystems/graphql-go.writePlannedField
```

Raw pprof files: `profile-graphql.cpu.pprof` + `profile-graphql.allocs.pprof` under the sweep out-dir; inspect interactively with `go tool pprof`.

## Scenario: `openapi`
pure OpenAPI/HTTP backend (hello_openapi); same Hello shape via HTTP/JSON.


- Endpoint: `http://localhost:18080/api/graphql`
- Duration per rep: `5.0s` × 3 reps (rep 1 discarded as warm-up)

| Target RPS | Achieved | Client mean | p50 | p95 | p99 | Gateway self (mean) | Dispatch (mean) |
|---:|---:|---:|---:|---:|---:|---:|---:|
| 1000 | 999 | 441µs | 443µs | 502µs | 639µs | 38µs | 207µs |
| 5000 | 4994 | 535µs | 517µs | 680µs | 984µs | 42µs | 251µs |
| 10000 | 9975 | 704µs | 646µs | 1.10ms | 2.22ms | 45µs | 316µs |
| 20000 | 19940 | 691µs | 619µs | 1.21ms | 2.81ms | 32µs | 260µs |
| 30000 | 29980 | 967µs | 845µs | 2.10ms | 3.24ms | 37µs | 298µs |
| 40000 | 39968 | 1.53ms | 1.22ms | 3.85ms | 6.13ms | 43µs | 439µs |
| 50000 | 48716 | 3.92ms | 2.53ms | 11.98ms | 19.95ms | 246µs | 929µs |
| 60000 | 56655 | 33.73ms | 46.77ms | 66.52ms | 108.73ms | 669µs | 1.56ms |

**Knee detected at 60000 RPS** (latency_above_50ms): p99 108728µs (108.7ms) exceeds 50ms SLA ceiling. Recommended ceiling: **50000 RPS** on this host.

### Interpretation

**~2030 RPS / core** across 24 logical cores at the recommended ceiling. Gateway self-time mean is **246µs** at the recommended ceiling — this is the per-request overhead the gateway adds on top of whatever the upstream takes.

### Where time + allocs go (50000 RPS, 20s CPU window)

**Top CPU (cumulative):**

```
File: gateway
Build ID: 54b2c8c4f700c66cdb23e4d0f7323b75a00d3de0
Type: cpu
Time: 2026-08-17 18:46:27 PDT
Duration: 20s, Total samples = 188.61s (943.02%)
Showing nodes accounting for 121.26s, 64.29% of 188.61s total
Dropped 1139 nodes (cum <= 0.94s)
      flat  flat%   sum%        cum   cum%
     0.59s  0.31%  0.31%    108.52s 57.54%  net/http.(*conn).serve
     0.05s 0.027%  0.34%     66.73s 35.38%  net/http.serverHandler.ServeHTTP
     0.14s 0.074%  0.41%     66.68s 35.35%  net/http.(*ServeMux).ServeHTTP
     0.08s 0.042%  0.46%     65.54s 34.75%  net/http.HandlerFunc.ServeHTTP
     0.45s  0.24%  0.69%     65.45s 34.70%  github.com/iodesystems/gwag/gw.(*Gateway).Handler.func1
     0.41s  0.22%  0.91%     62.35s 33.06%  github.com/iodesystems/gwag/gw.(*Gateway).serveGraphQLJSON
     0.15s  0.08%  0.99%     52.46s 27.81%  github.com/IodeSystems/graphql-go.ExecutePlanAppend
     0.03s 0.016%  1.01%     52.27s 27.71%  github.com/IodeSystems/graphql-go.ExecutePlanAppend.func1
     0.47s  0.25%  1.26%     52.21s 27.68%  github.com/IodeSystems/graphql-go.writePlannedSelection
     1.24s  0.66%  1.91%     51.72s 27.42%  github.com/IodeSystems/graphql-go.writePlannedField
```

**Top allocs (cumulative alloc_space):**

```
File: gateway
Build ID: 54b2c8c4f700c66cdb23e4d0f7323b75a00d3de0
Type: alloc_space
Time: 2026-08-17 18:46:47 PDT
Showing nodes accounting for 95580.16MB, 90.75% of 105326.68MB total
Dropped 569 nodes (cum <= 526.63MB)
      flat  flat%   sum%        cum   cum%
     134MB  0.13%  0.13% 91929.23MB 87.28%  net/http.(*conn).serve
         0     0%  0.13% 78728.64MB 74.75%  net/http.(*ServeMux).ServeHTTP
         0     0%  0.13% 78728.64MB 74.75%  net/http.HandlerFunc.ServeHTTP
         0     0%  0.13% 78728.64MB 74.75%  net/http.serverHandler.ServeHTTP
 1536.28MB  1.46%  1.59% 78599.69MB 74.62%  github.com/iodesystems/gwag/gw.(*Gateway).Handler.func1
         0     0%  1.59% 74600.80MB 70.83%  github.com/iodesystems/gwag/gw.(*Gateway).serveGraphQLJSON
         0     0%  1.59% 63248.25MB 60.05%  github.com/IodeSystems/graphql-go.ExecutePlanAppend
         0     0%  1.59% 63247.25MB 60.05%  github.com/IodeSystems/graphql-go.ExecutePlanAppend.func1
  403.02MB  0.38%  1.97% 63247.25MB 60.05%  github.com/IodeSystems/graphql-go.writePlannedSelection
  956.57MB  0.91%  2.88% 62844.23MB 59.67%  github.com/IodeSystems/graphql-go.writePlannedField
```

Raw pprof files: `profile-openapi.cpu.pprof` + `profile-openapi.allocs.pprof` under the sweep out-dir; inspect interactively with `go tool pprof`.

## Scenario: `proto`
pure proto/gRPC backend (greeter); baseline for native-format dispatch cost.


- Endpoint: `http://localhost:18080/api/graphql`
- Duration per rep: `5.0s` × 3 reps (rep 1 discarded as warm-up)

| Target RPS | Achieved | Client mean | p50 | p95 | p99 | Gateway self (mean) | Dispatch (mean) |
|---:|---:|---:|---:|---:|---:|---:|---:|
| 1000 | 1000 | 504µs | 508µs | 577µs | 764µs | 38µs | 255µs |
| 5000 | 4997 | 666µs | 620µs | 1.08ms | 1.35ms | 40µs | 337µs |
| 10000 | 9994 | 917µs | 796µs | 1.70ms | 2.28ms | 41µs | 393µs |
| 20000 | 19989 | 903µs | 799µs | 1.76ms | 2.40ms | 29µs | 337µs |
| 30000 | 29984 | 1.14ms | 1.01ms | 2.39ms | 3.40ms | 31µs | 433µs |
| 40000 | 39932 | 1.83ms | 1.39ms | 4.24ms | 10.30ms | 75µs | 633µs |
| 50000 | 49584 | 5.26ms | 3.25ms | 18.92ms | 27.02ms | 176µs | 1.21ms |
| 60000 | 57878 | 33.42ms | 45.12ms | 67.45ms | 106.66ms | 1.88ms | 2.02ms |

**Knee detected at 60000 RPS** (latency_above_50ms): p99 106657µs (106.7ms) exceeds 50ms SLA ceiling. Recommended ceiling: **50000 RPS** on this host.

### Interpretation

**~2066 RPS / core** across 24 logical cores at the recommended ceiling. Gateway self-time mean is **176µs** at the recommended ceiling — this is the per-request overhead the gateway adds on top of whatever the upstream takes.

### Where time + allocs go (50000 RPS, 20s CPU window)

**Top CPU (cumulative):**

```
File: gateway
Build ID: 54b2c8c4f700c66cdb23e4d0f7323b75a00d3de0
Type: cpu
Time: 2026-08-17 18:44:01 PDT
Duration: 20s, Total samples = 175.47s (877.32%)
Showing nodes accounting for 114.73s, 65.38% of 175.47s total
Dropped 1208 nodes (cum <= 0.88s)
      flat  flat%   sum%        cum   cum%
     0.49s  0.28%  0.28%    105.85s 60.32%  net/http.(*conn).serve
     0.09s 0.051%  0.33%     62.16s 35.42%  net/http.serverHandler.ServeHTTP
     0.10s 0.057%  0.39%     62.07s 35.37%  net/http.(*ServeMux).ServeHTTP
     0.08s 0.046%  0.43%     61.13s 34.84%  net/http.HandlerFunc.ServeHTTP
     0.33s  0.19%  0.62%     61.02s 34.78%  github.com/iodesystems/gwag/gw.(*Gateway).Handler.func1
     0.26s  0.15%  0.77%     58.32s 33.24%  github.com/iodesystems/gwag/gw.(*Gateway).serveGraphQLJSON
     0.14s  0.08%  0.85%     50.58s 28.83%  github.com/IodeSystems/graphql-go.ExecutePlanAppend
     0.04s 0.023%  0.87%     50.36s 28.70%  github.com/IodeSystems/graphql-go.ExecutePlanAppend.func1
     0.33s  0.19%  1.06%     50.25s 28.64%  github.com/IodeSystems/graphql-go.writePlannedSelection
     0.87s   0.5%  1.56%     49.92s 28.45%  github.com/IodeSystems/graphql-go.writePlannedField
```

**Top allocs (cumulative alloc_space):**

```
File: gateway
Build ID: 54b2c8c4f700c66cdb23e4d0f7323b75a00d3de0
Type: alloc_space
Time: 2026-08-17 18:44:21 PDT
Showing nodes accounting for 47194.91MB, 92.87% of 50816.07MB total
Dropped 446 nodes (cum <= 254.08MB)
      flat  flat%   sum%        cum   cum%
      63MB  0.12%  0.12% 43121.42MB 84.86%  net/http.(*conn).serve
         0     0%  0.12% 36537.74MB 71.90%  net/http.(*ServeMux).ServeHTTP
         0     0%  0.12% 36537.74MB 71.90%  net/http.HandlerFunc.ServeHTTP
         0     0%  0.12% 36537.74MB 71.90%  net/http.serverHandler.ServeHTTP
  750.14MB  1.48%  1.60% 36474.34MB 71.78%  github.com/iodesystems/gwag/gw.(*Gateway).Handler.func1
         0     0%  1.60% 34505.65MB 67.90%  github.com/iodesystems/gwag/gw.(*Gateway).serveGraphQLJSON
         0     0%  1.60% 28911.09MB 56.89%  github.com/IodeSystems/graphql-go.ExecutePlanAppend
         0     0%  1.60% 28910.59MB 56.89%  github.com/IodeSystems/graphql-go.ExecutePlanAppend.func1
  213.01MB  0.42%  2.02% 28910.59MB 56.89%  github.com/IodeSystems/graphql-go.writePlannedSelection
  292.02MB  0.57%  2.59% 28697.58MB 56.47%  github.com/IodeSystems/graphql-go.writePlannedField
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
