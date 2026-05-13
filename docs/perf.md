<!--
DO NOT EDIT — this file regenerates from sweep JSONs via 'bin/bench perf report'.
Run 'bin/bench perf all' to refresh the inputs under bench/.run/perf/.
-->

# Performance

> _Generated 2026-05-13T22:23:52Z from 3 scenario sweeps via `bin/bench perf report`._

**Headline (proto scenario, last healthy rung):** **48558 RPS** at p95 **15.17ms** with gateway self-time mean **353µs**.

> **Looking for "how does gwag compare to X?"** This page is gwag's
> own throughput on your hardware. For a head-to-head against
> graphql-mesh and Apollo Router on the same backends, see
> [`perf/comparison.md`](../perf/comparison.md).

## Machine

| Field | Value |
|---|---|
| Captured at | 2026-05-13T22:21:41Z |
| CPU | AMD Ryzen 9 3900X 12-Core Processor |
| Cores (logical) | 24 |
| RAM | 125.7 GiB |
| OS | Ubuntu 24.04 |
| Kernel | 6.8.0-111-generic |
| Arch | amd64 |
| Go | go1.26.2 |
| Gateway rev | 5a69765 (dirty) |


## Scenario: `graphql`

- Endpoint: `http://localhost:18080/api/graphql`
- Duration per rep: `5.0s` × 3 reps (rep 1 discarded as warm-up)

| Target RPS | Achieved | Client mean | p50 | p95 | p99 | Gateway self (mean) | Dispatch (mean) |
|---:|---:|---:|---:|---:|---:|---:|---:|
| 1000 | 999 | 476µs | 468µs | 545µs | 765µs | 39µs | 232µs |
| 5000 | 4997 | 595µs | 559µs | 889µs | 1.24ms | 44µs | 288µs |
| 10000 | 9994 | 814µs | 713µs | 1.54ms | 2.16ms | 44µs | 345µs |
| 20000 | 19900 | 798µs | 651µs | 1.42ms | 2.77ms | 32µs | 279µs |
| 30000 | 29738 | 1.20ms | 887µs | 2.70ms | 5.03ms | 38µs | 370µs |
| 40000 | 39222 | 2.69ms | 1.33ms | 7.13ms | 33.99ms | 91µs | 718µs |
| 50000 | 47037 | 18.75ms | 13.75ms | 48.68ms | 65.30ms | 5.68ms | 2.99ms |

**Knee detected at 50000 RPS** (latency_above_50ms): p99 65301µs (65.3ms) exceeds 50ms SLA ceiling. Recommended ceiling: **40000 RPS** on this host.

### Interpretation

**~1634 RPS / core** across 24 logical cores at the recommended ceiling. Gateway self-time mean is **91µs** at the recommended ceiling — this is the per-request overhead the gateway adds on top of whatever the upstream takes.

### Where time + allocs go (40000 RPS, 20s CPU window)

**Top CPU (cumulative):**

```
File: gateway
Build ID: 5797079ca43faef325f8559d3b49a99eb846659a
Type: cpu
Time: 2026-05-13 15:23:30 PDT
Duration: 20s, Total samples = 159.25s (796.19%)
Showing nodes accounting for 98.73s, 62.00% of 159.25s total
Dropped 1119 nodes (cum <= 0.80s)
      flat  flat%   sum%        cum   cum%
     0.29s  0.18%  0.18%     89.15s 55.98%  net/http.(*conn).serve
     0.08s  0.05%  0.23%     56.46s 35.45%  net/http.serverHandler.ServeHTTP
     0.05s 0.031%  0.26%     56.38s 35.40%  net/http.(*ServeMux).ServeHTTP
     0.04s 0.025%  0.29%     55.39s 34.78%  net/http.HandlerFunc.ServeHTTP
     0.22s  0.14%  0.43%     55.35s 34.76%  github.com/iodesystems/gwag/gw.(*Gateway).Handler.func1
     0.24s  0.15%  0.58%     52.26s 32.82%  github.com/iodesystems/gwag/gw.(*Gateway).serveGraphQLJSON
     0.12s 0.075%  0.65%     43.57s 27.36%  github.com/IodeSystems/graphql-go.ExecutePlanAppend
     0.01s 0.0063%  0.66%     43.44s 27.28%  github.com/IodeSystems/graphql-go.ExecutePlanAppend.func1
     0.50s  0.31%  0.97%     43.39s 27.25%  github.com/IodeSystems/graphql-go.writePlannedSelection
     0.91s  0.57%  1.54%     43.15s 27.10%  github.com/IodeSystems/graphql-go.writePlannedField
```

**Top allocs (cumulative alloc_space):**

```
File: gateway
Build ID: 5797079ca43faef325f8559d3b49a99eb846659a
Type: alloc_space
Time: 2026-05-13 15:23:50 PDT
Showing nodes accounting for 142147.63MB, 88.70% of 160259.53MB total
Dropped 792 nodes (cum <= 801.30MB)
      flat  flat%   sum%        cum   cum%
     180MB  0.11%  0.11% 142259.21MB 88.77%  net/http.(*conn).serve
         0     0%  0.11% 123887.75MB 77.30%  net/http.(*ServeMux).ServeHTTP
         0     0%  0.11% 123887.75MB 77.30%  net/http.HandlerFunc.ServeHTTP
         0     0%  0.11% 123887.75MB 77.30%  net/http.serverHandler.ServeHTTP
 2183.90MB  1.36%  1.48% 123309.04MB 76.94%  github.com/iodesystems/gwag/gw.(*Gateway).Handler.func1
         0     0%  1.48% 116328.94MB 72.59%  github.com/iodesystems/gwag/gw.(*Gateway).serveGraphQLJSON
    0.50MB 0.00031%  1.48% 94031.39MB 58.67%  github.com/IodeSystems/graphql-go.ExecutePlanAppend
         0     0%  1.48% 94030.39MB 58.67%  github.com/IodeSystems/graphql-go.ExecutePlanAppend.func1
  573.03MB  0.36%  1.83% 94030.39MB 58.67%  github.com/IodeSystems/graphql-go.writePlannedSelection
 1959.71MB  1.22%  3.06% 93457.37MB 58.32%  github.com/IodeSystems/graphql-go.writePlannedField
```

Raw pprof files: `profile-graphql.cpu.pprof` + `profile-graphql.allocs.pprof` under the sweep out-dir; inspect interactively with `go tool pprof`.

## Scenario: `openapi`
pure OpenAPI/HTTP backend (hello_openapi); same Hello shape via HTTP/JSON.


- Endpoint: `http://localhost:18080/api/graphql`
- Duration per rep: `5.0s` × 3 reps (rep 1 discarded as warm-up)

| Target RPS | Achieved | Client mean | p50 | p95 | p99 | Gateway self (mean) | Dispatch (mean) |
|---:|---:|---:|---:|---:|---:|---:|---:|
| 1000 | 999 | 455µs | 449µs | 522µs | 747µs | 41µs | 210µs |
| 5000 | 4996 | 570µs | 535µs | 832µs | 1.25ms | 47µs | 259µs |
| 10000 | 9995 | 785µs | 692µs | 1.51ms | 2.00ms | 49µs | 303µs |
| 20000 | 19898 | 735µs | 644µs | 1.42ms | 2.24ms | 35µs | 254µs |
| 30000 | 29769 | 1.08ms | 836µs | 2.28ms | 4.26ms | 37µs | 303µs |
| 40000 | 39559 | 1.75ms | 1.20ms | 4.50ms | 9.09ms | 49µs | 477µs |
| 50000 | 48204 | 5.52ms | 2.94ms | 18.83ms | 41.29ms | 172µs | 1.04ms |
| 60000 | 55808 | 33.22ms | 44.35ms | 68.06ms | 103.86ms | 1.09ms | 1.82ms |

**Knee detected at 60000 RPS** (latency_above_50ms): p99 103857µs (103.9ms) exceeds 50ms SLA ceiling. Recommended ceiling: **50000 RPS** on this host.

### Interpretation

**~2009 RPS / core** across 24 logical cores at the recommended ceiling. Gateway self-time mean is **172µs** at the recommended ceiling — this is the per-request overhead the gateway adds on top of whatever the upstream takes.

### Where time + allocs go (50000 RPS, 20s CPU window)

**Top CPU (cumulative):**

```
File: gateway
Build ID: 5797079ca43faef325f8559d3b49a99eb846659a
Type: cpu
Time: 2026-05-13 15:21:19 PDT
Duration: 20s, Total samples = 186.89s (934.43%)
Showing nodes accounting for 120.52s, 64.49% of 186.89s total
Dropped 1149 nodes (cum <= 0.93s)
      flat  flat%   sum%        cum   cum%
     0.52s  0.28%  0.28%    106.27s 56.86%  net/http.(*conn).serve
     0.04s 0.021%   0.3%        61s 32.64%  net/http.serverHandler.ServeHTTP
     0.06s 0.032%  0.33%     60.96s 32.62%  net/http.(*ServeMux).ServeHTTP
     0.04s 0.021%  0.35%     60.06s 32.14%  net/http.HandlerFunc.ServeHTTP
     0.26s  0.14%  0.49%     60.02s 32.12%  github.com/iodesystems/gwag/gw.(*Gateway).Handler.func1
     0.23s  0.12%  0.62%     56.65s 30.31%  github.com/iodesystems/gwag/gw.(*Gateway).serveGraphQLJSON
     0.18s 0.096%  0.71%     47.45s 25.39%  github.com/IodeSystems/graphql-go.ExecutePlanAppend
     0.10s 0.054%  0.77%     47.25s 25.28%  github.com/IodeSystems/graphql-go.ExecutePlanAppend.func1
     0.43s  0.23%     1%     47.11s 25.21%  github.com/IodeSystems/graphql-go.writePlannedSelection
     1.42s  0.76%  1.76%     46.89s 25.09%  github.com/IodeSystems/graphql-go.writePlannedField
```

**Top allocs (cumulative alloc_space):**

```
File: gateway
Build ID: 5797079ca43faef325f8559d3b49a99eb846659a
Type: alloc_space
Time: 2026-05-13 15:21:39 PDT
Showing nodes accounting for 101918.59MB, 89.54% of 113830.18MB total
Dropped 766 nodes (cum <= 569.15MB)
      flat  flat%   sum%        cum   cum%
  134.50MB  0.12%  0.12% 100332.82MB 88.14%  net/http.(*conn).serve
         0     0%  0.12% 87165.39MB 76.57%  net/http.(*ServeMux).ServeHTTP
         0     0%  0.12% 87165.39MB 76.57%  net/http.HandlerFunc.ServeHTTP
         0     0%  0.12% 87165.39MB 76.57%  net/http.serverHandler.ServeHTTP
 1563.79MB  1.37%  1.49% 86651.98MB 76.12%  github.com/iodesystems/gwag/gw.(*Gateway).Handler.func1
         0     0%  1.49% 81674.06MB 71.75%  github.com/iodesystems/gwag/gw.(*Gateway).serveGraphQLJSON
         0     0%  1.49% 65480.05MB 57.52%  github.com/IodeSystems/graphql-go.ExecutePlanAppend
         0     0%  1.49% 65479.55MB 57.52%  github.com/IodeSystems/graphql-go.ExecutePlanAppend.func1
  401.52MB  0.35%  1.84% 65479.55MB 57.52%  github.com/IodeSystems/graphql-go.writePlannedSelection
 1409.17MB  1.24%  3.08% 65078.03MB 57.17%  github.com/IodeSystems/graphql-go.writePlannedField
```

Raw pprof files: `profile-openapi.cpu.pprof` + `profile-openapi.allocs.pprof` under the sweep out-dir; inspect interactively with `go tool pprof`.

## Scenario: `proto`
pure proto/gRPC backend (greeter); baseline for native-format dispatch cost.


- Endpoint: `http://localhost:18080/api/graphql`
- Duration per rep: `5.0s` × 3 reps (rep 1 discarded as warm-up)

| Target RPS | Achieved | Client mean | p50 | p95 | p99 | Gateway self (mean) | Dispatch (mean) |
|---:|---:|---:|---:|---:|---:|---:|---:|
| 1000 | 999 | 513µs | 509µs | 596µs | 808µs | 39µs | 256µs |
| 5000 | 4998 | 656µs | 623µs | 978µs | 1.29ms | 42µs | 342µs |
| 10000 | 9992 | 860µs | 764µs | 1.57ms | 2.12ms | 42µs | 396µs |
| 20000 | 19915 | 802µs | 713µs | 1.48ms | 2.16ms | 29µs | 336µs |
| 30000 | 29739 | 1.26ms | 903µs | 2.37ms | 7.01ms | 45µs | 455µs |
| 40000 | 39531 | 1.87ms | 1.21ms | 4.16ms | 15.78ms | 51µs | 610µs |
| 50000 | 48559 | 4.83ms | 2.50ms | 15.17ms | 46.62ms | 353µs | 1.18ms |
| 60000 | 57109 | 33.38ms | 44.67ms | 66.10ms | 102.11ms | 1.63ms | 2.12ms |

**Knee detected at 60000 RPS** (latency_above_50ms): p99 102115µs (102.1ms) exceeds 50ms SLA ceiling. Recommended ceiling: **50000 RPS** on this host.

### Interpretation

**~2023 RPS / core** across 24 logical cores at the recommended ceiling. Gateway self-time mean is **353µs** at the recommended ceiling — this is the per-request overhead the gateway adds on top of whatever the upstream takes.

### Where time + allocs go (50000 RPS, 20s CPU window)

**Top CPU (cumulative):**

```
File: gateway
Build ID: 5797079ca43faef325f8559d3b49a99eb846659a
Type: cpu
Time: 2026-05-13 15:18:52 PDT
Duration: 20s, Total samples = 173.21s (866.02%)
Showing nodes accounting for 108.75s, 62.79% of 173.21s total
Dropped 1209 nodes (cum <= 0.87s)
      flat  flat%   sum%        cum   cum%
     0.62s  0.36%  0.36%    109.01s 62.94%  net/http.(*conn).serve
     0.10s 0.058%  0.42%     67.72s 39.10%  net/http.serverHandler.ServeHTTP
     0.06s 0.035%  0.45%     67.62s 39.04%  net/http.(*ServeMux).ServeHTTP
     0.12s 0.069%  0.52%     66.50s 38.39%  net/http.HandlerFunc.ServeHTTP
     0.29s  0.17%  0.69%     66.38s 38.32%  github.com/iodesystems/gwag/gw.(*Gateway).Handler.func1
     0.52s   0.3%  0.99%     62.89s 36.31%  github.com/iodesystems/gwag/gw.(*Gateway).serveGraphQLJSON
     0.18s   0.1%  1.09%     53.06s 30.63%  github.com/IodeSystems/graphql-go.ExecutePlanAppend
     0.06s 0.035%  1.13%     52.86s 30.52%  github.com/IodeSystems/graphql-go.ExecutePlanAppend.func1
     0.52s   0.3%  1.43%     52.77s 30.47%  github.com/IodeSystems/graphql-go.writePlannedSelection
     1.17s  0.68%  2.10%     52.49s 30.30%  github.com/IodeSystems/graphql-go.writePlannedField
```

**Top allocs (cumulative alloc_space):**

```
File: gateway
Build ID: 5797079ca43faef325f8559d3b49a99eb846659a
Type: alloc_space
Time: 2026-05-13 15:19:12 PDT
Showing nodes accounting for 49.72GB, 91.91% of 54.10GB total
Dropped 683 nodes (cum <= 0.27GB)
      flat  flat%   sum%        cum   cum%
    0.07GB  0.12%  0.12%    46.73GB 86.37%  net/http.(*conn).serve
         0     0%  0.12%    40.31GB 74.51%  net/http.(*ServeMux).ServeHTTP
         0     0%  0.12%    40.31GB 74.51%  net/http.HandlerFunc.ServeHTTP
         0     0%  0.12%    40.31GB 74.51%  net/http.serverHandler.ServeHTTP
    0.78GB  1.45%  1.57%    39.87GB 73.70%  github.com/iodesystems/gwag/gw.(*Gateway).Handler.func1
         0     0%  1.57%    37.38GB 69.09%  github.com/iodesystems/gwag/gw.(*Gateway).serveGraphQLJSON
         0     0%  1.57%    29.62GB 54.76%  github.com/IodeSystems/graphql-go.ExecutePlanAppend
         0     0%  1.57%    29.62GB 54.76%  github.com/IodeSystems/graphql-go.ExecutePlanAppend.func1
    0.20GB  0.38%  1.95%    29.62GB 54.76%  github.com/IodeSystems/graphql-go.writePlannedSelection
    0.67GB  1.24%  3.19%    29.42GB 54.39%  github.com/IodeSystems/graphql-go.writePlannedField
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
