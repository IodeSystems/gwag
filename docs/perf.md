<!--
DO NOT EDIT — this file regenerates from sweep JSONs via 'bin/bench perf report'.
Run 'bin/bench perf all' to refresh the inputs under bench/.run/perf/.
-->

# Performance

> _Generated 2026-05-25T19:53:44Z from 3 scenario sweeps via `bin/bench perf report`._

**Headline (proto scenario, last healthy rung):** **49290 RPS** at p95 **19.13ms** with gateway self-time mean **321µs**.

> **Looking for "how does gwag compare to X?"** This page is gwag's
> own throughput on your hardware. For a head-to-head against
> graphql-mesh and Apollo Router on the same backends, see
> [`perf/comparison.md`](../perf/comparison.md).

## Machine

| Field | Value |
|---|---|
| Captured at | 2026-05-25T19:51:33Z |
| CPU | AMD Ryzen 9 3900X 12-Core Processor |
| Cores (logical) | 24 |
| RAM | 125.7 GiB |
| OS | Ubuntu 24.04 |
| Kernel | 6.8.0-111-generic |
| Arch | amd64 |
| Go | go1.26.2 |
| Gateway rev | 57f694a (dirty) |


## Scenario: `graphql`

- Endpoint: `http://localhost:18080/api/graphql`
- Duration per rep: `5.0s` × 3 reps (rep 1 discarded as warm-up)

| Target RPS | Achieved | Client mean | p50 | p95 | p99 | Gateway self (mean) | Dispatch (mean) |
|---:|---:|---:|---:|---:|---:|---:|---:|
| 1000 | 999 | 474µs | 471µs | 543µs | 694µs | 38µs | 233µs |
| 5000 | 4992 | 585µs | 562µs | 746µs | 1.15ms | 42µs | 293µs |
| 10000 | 9962 | 698µs | 630µs | 1.14ms | 2.18ms | 39µs | 338µs |
| 20000 | 19847 | 1.12ms | 736µs | 2.12ms | 10.89ms | 53µs | 461µs |
| 30000 | 29965 | 1.09ms | 903µs | 2.68ms | 4.52ms | 36µs | 376µs |
| 40000 | 39858 | 1.93ms | 1.39ms | 5.44ms | 8.54ms | 48µs | 676µs |
| 50000 | 46970 | 20.46ms | 16.92ms | 50.58ms | 67.07ms | 6.18ms | 3.29ms |

**Knee detected at 50000 RPS** (latency_above_50ms): p99 67068µs (67.1ms) exceeds 50ms SLA ceiling. Recommended ceiling: **40000 RPS** on this host.

### Interpretation

**~1661 RPS / core** across 24 logical cores at the recommended ceiling. Gateway self-time mean is **48µs** at the recommended ceiling — this is the per-request overhead the gateway adds on top of whatever the upstream takes.

### Where time + allocs go (40000 RPS, 20s CPU window)

**Top CPU (cumulative):**

```
File: gateway
Build ID: 72b04885c57aa84066f4094a8278d116204a9f27
Type: cpu
Time: 2026-05-25 12:53:21 PDT
Duration: 20s, Total samples = 164.57s (822.81%)
Showing nodes accounting for 101.19s, 61.49% of 164.57s total
Dropped 1144 nodes (cum <= 0.82s)
      flat  flat%   sum%        cum   cum%
     0.24s  0.15%  0.15%     91.97s 55.89%  net/http.(*conn).serve
     0.05s  0.03%  0.18%     58.59s 35.60%  net/http.serverHandler.ServeHTTP
     0.03s 0.018%  0.19%     58.54s 35.57%  net/http.(*ServeMux).ServeHTTP
     0.07s 0.043%  0.24%     57.66s 35.04%  net/http.HandlerFunc.ServeHTTP
     0.21s  0.13%  0.36%     57.59s 34.99%  github.com/iodesystems/gwag/gw.(*Gateway).Handler.func1
     0.20s  0.12%  0.49%     54.74s 33.26%  github.com/iodesystems/gwag/gw.(*Gateway).serveGraphQLJSON
     0.17s   0.1%  0.59%     46.43s 28.21%  github.com/IodeSystems/graphql-go.ExecutePlanAppend
     0.04s 0.024%  0.61%     46.24s 28.10%  github.com/IodeSystems/graphql-go.ExecutePlanAppend.func1
     0.37s  0.22%  0.84%     46.14s 28.04%  github.com/IodeSystems/graphql-go.writePlannedSelection
     0.98s   0.6%  1.43%     45.78s 27.82%  github.com/IodeSystems/graphql-go.writePlannedField
```

**Top allocs (cumulative alloc_space):**

```
File: gateway
Build ID: 72b04885c57aa84066f4094a8278d116204a9f27
Type: alloc_space
Time: 2026-05-25 12:53:41 PDT
Showing nodes accounting for 152445.09MB, 89.31% of 170684.37MB total
Dropped 707 nodes (cum <= 853.42MB)
      flat  flat%   sum%        cum   cum%
  191.50MB  0.11%  0.11% 150833.49MB 88.37%  net/http.(*conn).serve
         0     0%  0.11% 131054.09MB 76.78%  net/http.(*ServeMux).ServeHTTP
         0     0%  0.11% 131054.09MB 76.78%  net/http.HandlerFunc.ServeHTTP
         0     0%  0.11% 131054.09MB 76.78%  net/http.serverHandler.ServeHTTP
 2299.92MB  1.35%  1.46% 130820.08MB 76.64%  github.com/iodesystems/gwag/gw.(*Gateway).Handler.func1
         0     0%  1.46% 123323.45MB 72.25%  github.com/iodesystems/gwag/gw.(*Gateway).serveGraphQLJSON
         0     0%  1.46% 99275.15MB 58.16%  github.com/IodeSystems/graphql-go.ExecutePlanAppend
         0     0%  1.46% 99275.15MB 58.16%  github.com/IodeSystems/graphql-go.ExecutePlanAppend.func1
  599.03MB  0.35%  1.81% 99275.15MB 58.16%  github.com/IodeSystems/graphql-go.writePlannedSelection
 1539.60MB   0.9%  2.71% 98676.12MB 57.81%  github.com/IodeSystems/graphql-go.writePlannedField
```

Raw pprof files: `profile-graphql.cpu.pprof` + `profile-graphql.allocs.pprof` under the sweep out-dir; inspect interactively with `go tool pprof`.

## Scenario: `openapi`
pure OpenAPI/HTTP backend (hello_openapi); same Hello shape via HTTP/JSON.


- Endpoint: `http://localhost:18080/api/graphql`
- Duration per rep: `5.0s` × 3 reps (rep 1 discarded as warm-up)

| Target RPS | Achieved | Client mean | p50 | p95 | p99 | Gateway self (mean) | Dispatch (mean) |
|---:|---:|---:|---:|---:|---:|---:|---:|
| 1000 | 999 | 463µs | 457µs | 555µs | 731µs | 41µs | 217µs |
| 5000 | 4996 | 559µs | 534µs | 757µs | 1.20ms | 46µs | 265µs |
| 10000 | 9990 | 696µs | 644µs | 1.11ms | 1.90ms | 46µs | 311µs |
| 20000 | 19977 | 694µs | 637µs | 1.21ms | 2.32ms | 34µs | 265µs |
| 30000 | 29979 | 1.02ms | 885µs | 2.26ms | 3.56ms | 39µs | 330µs |
| 40000 | 39895 | 1.85ms | 1.34ms | 5.04ms | 8.74ms | 60µs | 601µs |
| 50000 | 47949 | 13.61ms | 9.64ms | 40.38ms | 59.58ms | 2.08ms | 2.46ms |

**Knee detected at 50000 RPS** (latency_above_50ms): p99 59581µs (59.6ms) exceeds 50ms SLA ceiling. Recommended ceiling: **40000 RPS** on this host.

### Interpretation

**~1662 RPS / core** across 24 logical cores at the recommended ceiling. Gateway self-time mean is **60µs** at the recommended ceiling — this is the per-request overhead the gateway adds on top of whatever the upstream takes.

### Where time + allocs go (40000 RPS, 20s CPU window)

**Top CPU (cumulative):**

```
File: gateway
Build ID: 72b04885c57aa84066f4094a8278d116204a9f27
Type: cpu
Time: 2026-05-25 12:51:11 PDT
Duration: 20s, Total samples = 161.28s (806.36%)
Showing nodes accounting for 103.61s, 64.24% of 161.28s total
Dropped 1093 nodes (cum <= 0.81s)
      flat  flat%   sum%        cum   cum%
     0.34s  0.21%  0.21%     89.13s 55.26%  net/http.(*conn).serve
     0.07s 0.043%  0.25%     53.77s 33.34%  net/http.serverHandler.ServeHTTP
     0.05s 0.031%  0.29%     53.70s 33.30%  net/http.(*ServeMux).ServeHTTP
     0.03s 0.019%   0.3%     53.08s 32.91%  net/http.HandlerFunc.ServeHTTP
     0.42s  0.26%  0.56%     53.05s 32.89%  github.com/iodesystems/gwag/gw.(*Gateway).Handler.func1
     0.25s  0.16%  0.72%     50.19s 31.12%  github.com/iodesystems/gwag/gw.(*Gateway).serveGraphQLJSON
     0.17s  0.11%  0.82%     42.70s 26.48%  github.com/IodeSystems/graphql-go.ExecutePlanAppend
     0.02s 0.012%  0.84%     42.51s 26.36%  github.com/IodeSystems/graphql-go.ExecutePlanAppend.func1
     0.29s  0.18%  1.02%     42.46s 26.33%  github.com/IodeSystems/graphql-go.writePlannedSelection
     0.97s   0.6%  1.62%     42.28s 26.22%  github.com/IodeSystems/graphql-go.writePlannedField
```

**Top allocs (cumulative alloc_space):**

```
File: gateway
Build ID: 72b04885c57aa84066f4094a8278d116204a9f27
Type: alloc_space
Time: 2026-05-25 12:51:31 PDT
Showing nodes accounting for 110773.69MB, 88.89% of 124612.08MB total
Dropped 681 nodes (cum <= 623.06MB)
      flat  flat%   sum%        cum   cum%
     141MB  0.11%  0.11% 109230.03MB 87.66%  net/http.(*conn).serve
         0     0%  0.11% 94510.59MB 75.84%  net/http.(*ServeMux).ServeHTTP
         0     0%  0.11% 94510.59MB 75.84%  net/http.HandlerFunc.ServeHTTP
         0     0%  0.11% 94510.59MB 75.84%  net/http.serverHandler.ServeHTTP
 1706.81MB  1.37%  1.48% 94334.01MB 75.70%  github.com/iodesystems/gwag/gw.(*Gateway).Handler.func1
         0     0%  1.48% 88751.54MB 71.22%  github.com/iodesystems/gwag/gw.(*Gateway).serveGraphQLJSON
         0     0%  1.48% 70876.35MB 56.88%  github.com/IodeSystems/graphql-go.ExecutePlanAppend
         0     0%  1.48% 70876.35MB 56.88%  github.com/IodeSystems/graphql-go.ExecutePlanAppend.func1
  449.02MB  0.36%  1.84% 70876.35MB 56.88%  github.com/IodeSystems/graphql-go.writePlannedSelection
  991.06MB   0.8%  2.64% 70427.33MB 56.52%  github.com/IodeSystems/graphql-go.writePlannedField
```

Raw pprof files: `profile-openapi.cpu.pprof` + `profile-openapi.allocs.pprof` under the sweep out-dir; inspect interactively with `go tool pprof`.

## Scenario: `proto`
pure proto/gRPC backend (greeter); baseline for native-format dispatch cost.


- Endpoint: `http://localhost:18080/api/graphql`
- Duration per rep: `5.0s` × 3 reps (rep 1 discarded as warm-up)

| Target RPS | Achieved | Client mean | p50 | p95 | p99 | Gateway self (mean) | Dispatch (mean) |
|---:|---:|---:|---:|---:|---:|---:|---:|
| 1000 | 999 | 518µs | 516µs | 607µs | 758µs | 40µs | 260µs |
| 5000 | 4997 | 679µs | 635µs | 1.09ms | 1.38ms | 42µs | 340µs |
| 10000 | 9994 | 860µs | 749µs | 1.60ms | 2.15ms | 39µs | 364µs |
| 20000 | 19988 | 863µs | 757µs | 1.72ms | 2.39ms | 31µs | 347µs |
| 30000 | 29859 | 1.19ms | 984µs | 2.44ms | 3.67ms | 33µs | 436µs |
| 40000 | 39949 | 1.63ms | 1.34ms | 3.80ms | 5.51ms | 37µs | 576µs |
| 50000 | 49290 | 6.30ms | 4.32ms | 19.13ms | 29.50ms | 321µs | 1.50ms |
| 60000 | 57548 | 34.02ms | 46.72ms | 67.40ms | 107.60ms | 1.29ms | 2.19ms |

**Knee detected at 60000 RPS** (latency_above_50ms): p99 107602µs (107.6ms) exceeds 50ms SLA ceiling. Recommended ceiling: **50000 RPS** on this host.

### Interpretation

**~2054 RPS / core** across 24 logical cores at the recommended ceiling. Gateway self-time mean is **321µs** at the recommended ceiling — this is the per-request overhead the gateway adds on top of whatever the upstream takes.

### Where time + allocs go (50000 RPS, 20s CPU window)

**Top CPU (cumulative):**

```
File: gateway
Build ID: 72b04885c57aa84066f4094a8278d116204a9f27
Type: cpu
Time: 2026-05-25 12:49:00 PDT
Duration: 20.01s, Total samples = 175.42s (876.80%)
Showing nodes accounting for 109.45s, 62.39% of 175.42s total
Dropped 1231 nodes (cum <= 0.88s)
      flat  flat%   sum%        cum   cum%
     0.57s  0.32%  0.32%    114.92s 65.51%  net/http.(*conn).serve
     0.04s 0.023%  0.35%     69.02s 39.35%  net/http.serverHandler.ServeHTTP
     0.05s 0.029%  0.38%     68.99s 39.33%  net/http.(*ServeMux).ServeHTTP
     0.03s 0.017%  0.39%     68.19s 38.87%  net/http.HandlerFunc.ServeHTTP
     0.36s  0.21%   0.6%     68.15s 38.85%  github.com/iodesystems/gwag/gw.(*Gateway).Handler.func1
     0.30s  0.17%  0.77%     64.63s 36.84%  github.com/iodesystems/gwag/gw.(*Gateway).serveGraphQLJSON
     0.20s  0.11%  0.88%     55.42s 31.59%  github.com/IodeSystems/graphql-go.ExecutePlanAppend
         0     0%  0.88%     55.18s 31.46%  github.com/IodeSystems/graphql-go.ExecutePlanAppend.func1
     0.28s  0.16%  1.04%     55.15s 31.44%  github.com/IodeSystems/graphql-go.writePlannedSelection
     0.75s  0.43%  1.47%     54.85s 31.27%  github.com/IodeSystems/graphql-go.writePlannedField
```

**Top allocs (cumulative alloc_space):**

```
File: gateway
Build ID: 72b04885c57aa84066f4094a8278d116204a9f27
Type: alloc_space
Time: 2026-05-25 12:49:20 PDT
Showing nodes accounting for 71.38GB, 91.91% of 77.66GB total
Dropped 603 nodes (cum <= 0.39GB)
      flat  flat%   sum%        cum   cum%
    0.09GB  0.12%  0.12%    67.02GB 86.30%  net/http.(*conn).serve
         0     0%  0.12%    57.60GB 74.17%  net/http.(*ServeMux).ServeHTTP
         0     0%  0.12%    57.60GB 74.17%  net/http.HandlerFunc.ServeHTTP
         0     0%  0.12%    57.60GB 74.17%  net/http.serverHandler.ServeHTTP
    1.09GB  1.41%  1.53%    57.48GB 74.01%  github.com/iodesystems/gwag/gw.(*Gateway).Handler.func1
         0     0%  1.53%    53.94GB 69.46%  github.com/iodesystems/gwag/gw.(*Gateway).serveGraphQLJSON
         0     0%  1.53%    42.64GB 54.91%  github.com/IodeSystems/graphql-go.ExecutePlanAppend
         0     0%  1.53%    42.64GB 54.91%  github.com/IodeSystems/graphql-go.ExecutePlanAppend.func1
    0.29GB  0.37%  1.90%    42.64GB 54.91%  github.com/IodeSystems/graphql-go.writePlannedSelection
    0.44GB  0.57%  2.47%    42.35GB 54.53%  github.com/IodeSystems/graphql-go.writePlannedField
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
