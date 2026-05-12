<!--
DO NOT EDIT — this file regenerates from sweep JSONs via 'bin/bench perf report'.
Run 'bin/bench perf all' to refresh the inputs under bench/.run/perf/.
-->

# Performance

> _Generated 2026-05-12T00:17:52Z from 3 scenario sweeps via `bin/bench perf report`._

**Headline (proto scenario, last healthy rung):** **48707 RPS** at p95 **22.59ms** with gateway self-time mean **225µs**.

## Machine

| Field | Value |
|---|---|
| Captured at | 2026-05-12T00:15:40Z |
| CPU | AMD Ryzen 9 3900X 12-Core Processor |
| Cores (logical) | 24 |
| RAM | 125.7 GiB |
| OS | Ubuntu 24.04 |
| Kernel | 6.8.0-111-generic |
| Arch | amd64 |
| Go | go1.26.2 |
| Gateway rev | a52036e (dirty) |


## Scenario: `graphql`

- Endpoint: `http://localhost:18080/api/graphql`
- Duration per rep: `5.0s` × 3 reps (rep 1 discarded as warm-up)

| Target RPS | Achieved | Client mean | p50 | p95 | p99 | Gateway self (mean) | Dispatch (mean) |
|---:|---:|---:|---:|---:|---:|---:|---:|
| 1000 | 1000 | 466µs | 464µs | 567µs | 871µs | 38µs | 229µs |
| 5000 | 4980 | 669µs | 569µs | 1.25ms | 2.83ms | 44µs | 323µs |
| 10000 | 9994 | 719µs | 625µs | 1.38ms | 1.94ms | 38µs | 333µs |
| 20000 | 19926 | 833µs | 717µs | 1.70ms | 2.88ms | 35µs | 317µs |
| 30000 | 29623 | 1.41ms | 959µs | 3.27ms | 8.35ms | 45µs | 453µs |
| 40000 | 39140 | 2.81ms | 1.53ms | 8.56ms | 21.70ms | 182µs | 925µs |
| 50000 | 46267 | 15.89ms | 14.24ms | 38.79ms | 55.98ms | 2.85ms | 3.05ms |

**Knee detected at 50000 RPS** (latency_above_50ms): p99 55976µs (56.0ms) exceeds 50ms SLA ceiling. Recommended ceiling: **40000 RPS** on this host.

### Interpretation

**~1631 RPS / core** across 24 logical cores at the recommended ceiling. Gateway self-time mean is **182µs** at the recommended ceiling — this is the per-request overhead the gateway adds on top of whatever the upstream takes.

### Where time + allocs go (40000 RPS, 20s CPU window)

**Top CPU (cumulative):**

```
File: gateway
Build ID: f3cdb232e35c0156b99037131c8136997517ec51
Type: cpu
Time: 2026-05-11 17:17:29 PDT
Duration: 20s, Total samples = 156s (779.97%)
Showing nodes accounting for 100.20s, 64.23% of 156s total
Dropped 1087 nodes (cum <= 0.78s)
      flat  flat%   sum%        cum   cum%
     0.32s  0.21%  0.21%     85.42s 54.76%  net/http.(*conn).serve
     0.05s 0.032%  0.24%     49.90s 31.99%  net/http.serverHandler.ServeHTTP
     0.08s 0.051%  0.29%     49.85s 31.96%  net/http.(*ServeMux).ServeHTTP
     0.02s 0.013%   0.3%     48.99s 31.40%  net/http.HandlerFunc.ServeHTTP
     0.28s  0.18%  0.48%     48.97s 31.39%  github.com/iodesystems/gwag/gw.(*Gateway).Handler.func1
     0.24s  0.15%  0.63%     46.76s 29.97%  github.com/iodesystems/gwag/gw.(*Gateway).serveGraphQLJSON
     0.06s 0.038%  0.67%     40.16s 25.74%  github.com/graphql-go/graphql.ExecutePlanAppend
     0.08s 0.051%  0.72%     40.02s 25.65%  github.com/graphql-go/graphql.ExecutePlanAppend.func1
     0.42s  0.27%  0.99%     39.90s 25.58%  github.com/graphql-go/graphql.writePlannedSelection
     0.94s   0.6%  1.60%     39.64s 25.41%  github.com/graphql-go/graphql.writePlannedField
```

**Top allocs (cumulative alloc_space):**

```
File: gateway
Build ID: f3cdb232e35c0156b99037131c8136997517ec51
Type: alloc_space
Time: 2026-05-11 17:17:49 PDT
Showing nodes accounting for 322450.37MB, 89.12% of 361800.20MB total
Dropped 799 nodes (cum <= 1809MB)
      flat  flat%   sum%        cum   cum%
  436.51MB  0.12%  0.12% 318796.12MB 88.11%  net/http.(*conn).serve
         0     0%  0.12% 274543.56MB 75.88%  net/http.(*ServeMux).ServeHTTP
         0     0%  0.12% 274543.56MB 75.88%  net/http.HandlerFunc.ServeHTTP
         0     0%  0.12% 274543.56MB 75.88%  net/http.serverHandler.ServeHTTP
         0     0%  0.12% 273979.05MB 75.73%  github.com/iodesystems/gwag/gw.(*Gateway).Handler.func1
         0     0%  0.12% 267145.75MB 73.84%  github.com/iodesystems/gwag/gw.(*Gateway).serveGraphQLJSON
         0     0%  0.12% 214287.24MB 59.23%  github.com/graphql-go/graphql.ExecutePlanAppend
         0     0%  0.12% 212974.18MB 58.87%  github.com/graphql-go/graphql.ExecutePlanAppend.func1
 1293.56MB  0.36%  0.48% 212974.18MB 58.87%  github.com/graphql-go/graphql.writePlannedSelection
 4626.74MB  1.28%  1.76% 211680.62MB 58.51%  github.com/graphql-go/graphql.writePlannedField
```

Raw pprof files: `profile-graphql.cpu.pprof` + `profile-graphql.allocs.pprof` under the sweep out-dir; inspect interactively with `go tool pprof`.

## Scenario: `openapi`
pure OpenAPI/HTTP backend (hello_openapi); same Hello shape via HTTP/JSON.


- Endpoint: `http://localhost:18080/api/graphql`
- Duration per rep: `5.0s` × 3 reps (rep 1 discarded as warm-up)

| Target RPS | Achieved | Client mean | p50 | p95 | p99 | Gateway self (mean) | Dispatch (mean) |
|---:|---:|---:|---:|---:|---:|---:|---:|
| 1000 | 999 | 456µs | 450µs | 562µs | 834µs | 41µs | 211µs |
| 5000 | 4996 | 555µs | 523µs | 852µs | 1.28ms | 47µs | 252µs |
| 10000 | 9995 | 757µs | 662µs | 1.49ms | 2.11ms | 47µs | 295µs |
| 20000 | 19879 | 756µs | 630µs | 1.44ms | 2.40ms | 36µs | 250µs |
| 30000 | 29743 | 1.20ms | 837µs | 2.61ms | 8.11ms | 51µs | 340µs |
| 40000 | 39302 | 2.24ms | 1.21ms | 5.82ms | 24.10ms | 100µs | 581µs |
| 50000 | 47933 | 5.77ms | 2.63ms | 23.49ms | 45.01ms | 589µs | 1.15ms |
| 60000 | 55064 | 35.31ms | 44.81ms | 76.05ms | 109.41ms | 3.06ms | 2.32ms |

**Knee detected at 60000 RPS** (latency_above_50ms): p99 109414µs (109.4ms) exceeds 50ms SLA ceiling. Recommended ceiling: **50000 RPS** on this host.

### Interpretation

**~1997 RPS / core** across 24 logical cores at the recommended ceiling. Gateway self-time mean is **589µs** at the recommended ceiling — this is the per-request overhead the gateway adds on top of whatever the upstream takes.

### Where time + allocs go (50000 RPS, 20s CPU window)

**Top CPU (cumulative):**

```
File: gateway
Build ID: f3cdb232e35c0156b99037131c8136997517ec51
Type: cpu
Time: 2026-05-11 17:15:18 PDT
Duration: 20s, Total samples = 180.19s (900.93%)
Showing nodes accounting for 114.52s, 63.56% of 180.19s total
Dropped 1106 nodes (cum <= 0.90s)
      flat  flat%   sum%        cum   cum%
     0.46s  0.26%  0.26%    103.71s 57.56%  net/http.(*conn).serve
     0.11s 0.061%  0.32%     60.49s 33.57%  net/http.serverHandler.ServeHTTP
     0.07s 0.039%  0.36%     60.38s 33.51%  net/http.(*ServeMux).ServeHTTP
     0.04s 0.022%  0.38%     59.50s 33.02%  net/http.HandlerFunc.ServeHTTP
     0.24s  0.13%  0.51%     59.46s 33.00%  github.com/iodesystems/gwag/gw.(*Gateway).Handler.func1
     0.35s  0.19%   0.7%     57.06s 31.67%  github.com/iodesystems/gwag/gw.(*Gateway).serveGraphQLJSON
     0.10s 0.055%  0.76%     48.50s 26.92%  github.com/graphql-go/graphql.ExecutePlanAppend
     0.07s 0.039%   0.8%     48.25s 26.78%  github.com/graphql-go/graphql.ExecutePlanAppend.func1
     0.57s  0.32%  1.12%     48.08s 26.68%  github.com/graphql-go/graphql.writePlannedSelection
     1.67s  0.93%  2.04%     47.80s 26.53%  github.com/graphql-go/graphql.writePlannedField
```

**Top allocs (cumulative alloc_space):**

```
File: gateway
Build ID: f3cdb232e35c0156b99037131c8136997517ec51
Type: alloc_space
Time: 2026-05-11 17:15:38 PDT
Showing nodes accounting for 283677.14MB, 89.32% of 317579.39MB total
Dropped 794 nodes (cum <= 1587.90MB)
      flat  flat%   sum%        cum   cum%
  391.01MB  0.12%  0.12% 278956.59MB 87.84%  net/http.(*conn).serve
         0     0%  0.12% 239935.65MB 75.55%  net/http.(*ServeMux).ServeHTTP
         0     0%  0.12% 239935.65MB 75.55%  net/http.HandlerFunc.ServeHTTP
         0     0%  0.12% 239935.65MB 75.55%  net/http.serverHandler.ServeHTTP
         0     0%  0.12% 239423.03MB 75.39%  github.com/iodesystems/gwag/gw.(*Gateway).Handler.func1
         0     0%  0.12% 233383.26MB 73.49%  github.com/iodesystems/gwag/gw.(*Gateway).serveGraphQLJSON
         0     0%  0.12% 186591.52MB 58.75%  github.com/graphql-go/graphql.ExecutePlanAppend
         0     0%  0.12% 185429.97MB 58.39%  github.com/graphql-go/graphql.ExecutePlanAppend.func1
 1135.05MB  0.36%  0.48% 185429.97MB 58.39%  github.com/graphql-go/graphql.writePlannedSelection
 4098.21MB  1.29%  1.77% 184294.92MB 58.03%  github.com/graphql-go/graphql.writePlannedField
```

Raw pprof files: `profile-openapi.cpu.pprof` + `profile-openapi.allocs.pprof` under the sweep out-dir; inspect interactively with `go tool pprof`.

## Scenario: `proto`
pure proto/gRPC backend (greeter); baseline for native-format dispatch cost.


- Endpoint: `http://localhost:18080/api/graphql`
- Duration per rep: `5.0s` × 3 reps (rep 1 discarded as warm-up)

| Target RPS | Achieved | Client mean | p50 | p95 | p99 | Gateway self (mean) | Dispatch (mean) |
|---:|---:|---:|---:|---:|---:|---:|---:|
| 1000 | 999 | 511µs | 509µs | 606µs | 887µs | 39µs | 255µs |
| 5000 | 4998 | 647µs | 622µs | 957µs | 1.32ms | 41µs | 338µs |
| 10000 | 9983 | 764µs | 683µs | 1.41ms | 1.99ms | 37µs | 354µs |
| 20000 | 19861 | 834µs | 722µs | 1.50ms | 2.48ms | 30µs | 364µs |
| 30000 | 29653 | 1.28ms | 986µs | 2.69ms | 5.39ms | 36µs | 487µs |
| 40000 | 39386 | 2.22ms | 1.41ms | 5.43ms | 15.55ms | 44µs | 786µs |
| 50000 | 48708 | 6.56ms | 3.99ms | 22.59ms | 40.01ms | 225µs | 1.42ms |
| 60000 | 56748 | 33.67ms | 44.60ms | 67.40ms | 103.07ms | 2.15ms | 2.14ms |

**Knee detected at 60000 RPS** (latency_above_50ms): p99 103068µs (103.1ms) exceeds 50ms SLA ceiling. Recommended ceiling: **50000 RPS** on this host.

### Interpretation

**~2029 RPS / core** across 24 logical cores at the recommended ceiling. Gateway self-time mean is **225µs** at the recommended ceiling — this is the per-request overhead the gateway adds on top of whatever the upstream takes.

### Where time + allocs go (50000 RPS, 20s CPU window)

**Top CPU (cumulative):**

```
File: gateway
Build ID: f3cdb232e35c0156b99037131c8136997517ec51
Type: cpu
Time: 2026-05-11 17:12:51 PDT
Duration: 20s, Total samples = 163.34s (816.64%)
Showing nodes accounting for 101.23s, 61.98% of 163.34s total
Dropped 1182 nodes (cum <= 0.82s)
      flat  flat%   sum%        cum   cum%
     0.56s  0.34%  0.34%    104.90s 64.22%  net/http.(*conn).serve
     0.05s 0.031%  0.37%     60.26s 36.89%  net/http.serverHandler.ServeHTTP
     0.06s 0.037%  0.41%     60.21s 36.86%  net/http.(*ServeMux).ServeHTTP
     0.08s 0.049%  0.46%     59.40s 36.37%  net/http.HandlerFunc.ServeHTTP
     0.19s  0.12%  0.58%     59.30s 36.30%  github.com/iodesystems/gwag/gw.(*Gateway).Handler.func1
     0.31s  0.19%  0.77%     56.87s 34.82%  github.com/iodesystems/gwag/gw.(*Gateway).serveGraphQLJSON
     0.14s 0.086%  0.85%     49.31s 30.19%  github.com/graphql-go/graphql.ExecutePlanAppend
     0.08s 0.049%   0.9%     49.04s 30.02%  github.com/graphql-go/graphql.ExecutePlanAppend.func1
     0.54s  0.33%  1.23%     48.85s 29.91%  github.com/graphql-go/graphql.writePlannedSelection
     1.42s  0.87%  2.10%     48.53s 29.71%  github.com/graphql-go/graphql.writePlannedField
```

**Top allocs (cumulative alloc_space):**

```
File: gateway
Build ID: f3cdb232e35c0156b99037131c8136997517ec51
Type: alloc_space
Time: 2026-05-11 17:13:11 PDT
Showing nodes accounting for 231646.66MB, 88.24% of 262529.47MB total
Dropped 787 nodes (cum <= 1312.65MB)
      flat  flat%   sum%        cum   cum%
  322.01MB  0.12%  0.12% 229688.83MB 87.49%  net/http.(*conn).serve
         0     0%  0.12% 197218.48MB 75.12%  net/http.(*ServeMux).ServeHTTP
         0     0%  0.12% 197218.48MB 75.12%  net/http.HandlerFunc.ServeHTTP
         0     0%  0.12% 197218.48MB 75.12%  net/http.serverHandler.ServeHTTP
         0     0%  0.12% 196772.49MB 74.95%  github.com/iodesystems/gwag/gw.(*Gateway).Handler.func1
         0     0%  0.12% 191737.27MB 73.03%  github.com/iodesystems/gwag/gw.(*Gateway).serveGraphQLJSON
         0     0%  0.12% 153030.96MB 58.29%  github.com/graphql-go/graphql.ExecutePlanAppend
         0     0%  0.12% 152059.91MB 57.92%  github.com/graphql-go/graphql.ExecutePlanAppend.func1
  941.04MB  0.36%  0.48% 152059.91MB 57.92%  github.com/graphql-go/graphql.writePlannedSelection
 3408.17MB  1.30%  1.78% 151118.87MB 57.56%  github.com/graphql-go/graphql.writePlannedField
```

Raw pprof files: `profile-proto.cpu.pprof` + `profile-proto.allocs.pprof` under the sweep out-dir; inspect interactively with `go tool pprof`.


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
