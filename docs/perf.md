<!--
DO NOT EDIT — this file regenerates from sweep JSONs via 'bin/bench perf report'.
Run 'bin/bench perf all' to refresh the inputs under bench/.run/perf/.
-->

# Performance

> _Generated 2026-05-11T23:47:01Z from 3 scenario sweeps via `bin/bench perf report`._

**Headline (proto scenario, last healthy rung):** **48672 RPS** at p95 **19.97ms** with gateway self-time mean **109µs**.

## Machine

| Field | Value |
|---|---|
| Captured at | 2026-05-11T23:45:21Z |
| CPU | AMD Ryzen 9 3900X 12-Core Processor |
| Cores (logical) | 24 |
| RAM | 125.7 GiB |
| OS | Ubuntu 24.04 |
| Kernel | 6.8.0-111-generic |
| Arch | amd64 |
| Go | go1.26.2 |
| Gateway rev | 9157d6b (dirty) |


## Scenario: `graphql`

- Endpoint: `http://localhost:18080/api/graphql`
- Duration per rep: `5.0s` × 3 reps (rep 1 discarded as warm-up)

| Target RPS | Achieved | Client mean | p50 | p95 | p99 | Gateway self (mean) | Dispatch (mean) |
|---:|---:|---:|---:|---:|---:|---:|---:|
| 1000 | 999 | 758µs | 722µs | 1.18ms | 1.51ms | 39µs | 518µs |
| 5000 | 4997 | 1.29ms | 1.26ms | 1.98ms | 2.43ms | 37µs | 1.02ms |
| 10000 | 9995 | 1.52ms | 1.39ms | 2.71ms | 3.96ms | 31µs | 1.24ms |
| 20000 | 19828 | 2.44ms | 1.73ms | 6.16ms | 11.46ms | 39µs | 1.73ms |
| 30000 | 28227 | 49.77ms | 47.11ms | 79.26ms | 100.05ms | 38.73ms | 9.33ms |

**Knee detected at 30000 RPS** (latency_above_50ms): p99 100052µs (100.1ms) exceeds 50ms SLA ceiling. Recommended ceiling: **20000 RPS** on this host.

### Interpretation

**~826 RPS / core** across 24 logical cores at the recommended ceiling. Gateway self-time mean is **39µs** at the recommended ceiling — this is the per-request overhead the gateway adds on top of whatever the upstream takes.

### Where time + allocs go (20000 RPS, 20s CPU window)

**Top CPU (cumulative):**

```
File: gateway
Build ID: f3cdb232e35c0156b99037131c8136997517ec51
Type: cpu
Time: 2026-05-11 16:46:39 PDT
Duration: 20s, Total samples = 83.41s (417.03%)
Showing nodes accounting for 52.68s, 63.16% of 83.41s total
Dropped 991 nodes (cum <= 0.42s)
      flat  flat%   sum%        cum   cum%
     0.25s   0.3%   0.3%     44.77s 53.67%  net/http.(*conn).serve
     0.05s  0.06%  0.36%     28.75s 34.47%  net/http.serverHandler.ServeHTTP
     0.02s 0.024%  0.38%     28.70s 34.41%  net/http.(*ServeMux).ServeHTTP
     0.01s 0.012%   0.4%     28.18s 33.78%  net/http.HandlerFunc.ServeHTTP
     0.15s  0.18%  0.58%     28.17s 33.77%  github.com/iodesystems/gwag/gw.(*Gateway).Handler.func1
     0.07s 0.084%  0.66%     27.09s 32.48%  github.com/iodesystems/gwag/gw.(*Gateway).serveGraphQLJSON
     0.09s  0.11%  0.77%     22.60s 27.10%  github.com/graphql-go/graphql.ExecutePlanAppend
     0.04s 0.048%  0.82%     22.43s 26.89%  github.com/graphql-go/graphql.ExecutePlanAppend.func1
     0.21s  0.25%  1.07%     22.37s 26.82%  github.com/graphql-go/graphql.writePlannedSelection
     0.59s  0.71%  1.77%     22.22s 26.64%  github.com/graphql-go/graphql.writePlannedField
```

**Top allocs (cumulative alloc_space):**

```
File: gateway
Build ID: f3cdb232e35c0156b99037131c8136997517ec51
Type: alloc_space
Time: 2026-05-11 16:46:59 PDT
Showing nodes accounting for 173289.82MB, 89.16% of 194349.40MB total
Dropped 712 nodes (cum <= 971.75MB)
      flat  flat%   sum%        cum   cum%
     232MB  0.12%  0.12% 170261.47MB 87.61%  net/http.(*conn).serve
         0     0%  0.12% 146265.75MB 75.26%  net/http.(*ServeMux).ServeHTTP
         0     0%  0.12% 146265.75MB 75.26%  net/http.HandlerFunc.ServeHTTP
         0     0%  0.12% 146265.75MB 75.26%  net/http.serverHandler.ServeHTTP
         0     0%  0.12% 145952.16MB 75.10%  github.com/iodesystems/gwag/gw.(*Gateway).Handler.func1
         0     0%  0.12% 142247.99MB 73.19%  github.com/iodesystems/gwag/gw.(*Gateway).serveGraphQLJSON
         0     0%  0.12% 113545.49MB 58.42%  github.com/graphql-go/graphql.ExecutePlanAppend
         0     0%  0.12% 112834.46MB 58.06%  github.com/graphql-go/graphql.ExecutePlanAppend.func1
  704.53MB  0.36%  0.48% 112834.46MB 58.06%  github.com/graphql-go/graphql.writePlannedSelection
 2525.63MB  1.30%  1.78% 112129.93MB 57.70%  github.com/graphql-go/graphql.writePlannedField
```

Raw pprof files: `profile-graphql.cpu.pprof` + `profile-graphql.allocs.pprof` under the sweep out-dir; inspect interactively with `go tool pprof`.

## Scenario: `openapi`
pure OpenAPI/HTTP backend (hello_openapi); same Hello shape via HTTP/JSON.


- Endpoint: `http://localhost:18080/api/graphql`
- Duration per rep: `5.0s` × 3 reps (rep 1 discarded as warm-up)

| Target RPS | Achieved | Client mean | p50 | p95 | p99 | Gateway self (mean) | Dispatch (mean) |
|---:|---:|---:|---:|---:|---:|---:|---:|
| 1000 | 999 | 445µs | 444µs | 514µs | 755µs | 40µs | 206µs |
| 5000 | 4997 | 542µs | 518µs | 798µs | 1.17ms | 45µs | 247µs |
| 10000 | 9994 | 763µs | 675µs | 1.48ms | 1.98ms | 47µs | 305µs |
| 20000 | 19895 | 724µs | 617µs | 1.32ms | 2.20ms | 35µs | 240µs |
| 30000 | 29767 | 1.12ms | 806µs | 2.69ms | 5.63ms | 37µs | 323µs |
| 40000 | 39308 | 2.09ms | 1.20ms | 5.56ms | 18.66ms | 67µs | 544µs |
| 50000 | 48002 | 5.63ms | 2.79ms | 23.65ms | 38.75ms | 208µs | 994µs |
| 60000 | 55525 | 34.35ms | 44.84ms | 74.86ms | 105.23ms | 2.69ms | 1.81ms |

**Knee detected at 60000 RPS** (latency_above_50ms): p99 105234µs (105.2ms) exceeds 50ms SLA ceiling. Recommended ceiling: **50000 RPS** on this host.

### Interpretation

**~2000 RPS / core** across 24 logical cores at the recommended ceiling. Gateway self-time mean is **208µs** at the recommended ceiling — this is the per-request overhead the gateway adds on top of whatever the upstream takes.

### Where time + allocs go (50000 RPS, 20s CPU window)

**Top CPU (cumulative):**

```
File: gateway
Build ID: f3cdb232e35c0156b99037131c8136997517ec51
Type: cpu
Time: 2026-05-11 16:44:58 PDT
Duration: 20s, Total samples = 183.61s (918.04%)
Showing nodes accounting for 116.94s, 63.69% of 183.61s total
Dropped 1126 nodes (cum <= 0.92s)
      flat  flat%   sum%        cum   cum%
     0.45s  0.25%  0.25%    102.63s 55.90%  net/http.(*conn).serve
     0.03s 0.016%  0.26%     58.66s 31.95%  net/http.serverHandler.ServeHTTP
     0.09s 0.049%  0.31%     58.63s 31.93%  net/http.(*ServeMux).ServeHTTP
     0.04s 0.022%  0.33%     57.72s 31.44%  net/http.HandlerFunc.ServeHTTP
     0.18s 0.098%  0.43%     57.66s 31.40%  github.com/iodesystems/gwag/gw.(*Gateway).Handler.func1
     0.28s  0.15%  0.58%     55.43s 30.19%  github.com/iodesystems/gwag/gw.(*Gateway).serveGraphQLJSON
     0.14s 0.076%  0.66%     46.60s 25.38%  github.com/graphql-go/graphql.ExecutePlanAppend
     0.08s 0.044%   0.7%     46.38s 25.26%  github.com/graphql-go/graphql.ExecutePlanAppend.func1
     0.60s  0.33%  1.03%     46.19s 25.16%  github.com/graphql-go/graphql.writePlannedSelection
     1.46s   0.8%  1.82%     45.88s 24.99%  github.com/graphql-go/graphql.writePlannedField
```

**Top allocs (cumulative alloc_space):**

```
File: gateway
Build ID: f3cdb232e35c0156b99037131c8136997517ec51
Type: alloc_space
Time: 2026-05-11 16:45:18 PDT
Showing nodes accounting for 156227MB, 89.68% of 174201.71MB total
Dropped 686 nodes (cum <= 871.01MB)
      flat  flat%   sum%        cum   cum%
     209MB  0.12%  0.12% 152132.64MB 87.33%  net/http.(*conn).serve
         0     0%  0.12% 130420.11MB 74.87%  net/http.(*ServeMux).ServeHTTP
         0     0%  0.12% 130420.11MB 74.87%  net/http.HandlerFunc.ServeHTTP
         0     0%  0.12% 130420.11MB 74.87%  net/http.serverHandler.ServeHTTP
         0     0%  0.12% 130142.90MB 74.71%  github.com/iodesystems/gwag/gw.(*Gateway).Handler.func1
         0     0%  0.12% 126800.75MB 72.79%  github.com/iodesystems/gwag/gw.(*Gateway).serveGraphQLJSON
         0     0%  0.12% 100845.77MB 57.89%  github.com/graphql-go/graphql.ExecutePlanAppend
         0     0%  0.12% 100208.24MB 57.52%  github.com/graphql-go/graphql.ExecutePlanAppend.func1
  636.53MB  0.37%  0.49% 100208.24MB 57.52%  github.com/graphql-go/graphql.writePlannedSelection
 2290.61MB  1.31%  1.80% 99571.71MB 57.16%  github.com/graphql-go/graphql.writePlannedField
```

Raw pprof files: `profile-openapi.cpu.pprof` + `profile-openapi.allocs.pprof` under the sweep out-dir; inspect interactively with `go tool pprof`.

## Scenario: `proto`
pure proto/gRPC backend (greeter); baseline for native-format dispatch cost.


- Endpoint: `http://localhost:18080/api/graphql`
- Duration per rep: `5.0s` × 3 reps (rep 1 discarded as warm-up)

| Target RPS | Achieved | Client mean | p50 | p95 | p99 | Gateway self (mean) | Dispatch (mean) |
|---:|---:|---:|---:|---:|---:|---:|---:|
| 1000 | 999 | 529µs | 529µs | 648µs | 865µs | 41µs | 401µs |
| 5000 | 4997 | 610µs | 587µs | 925µs | 1.28ms | 38µs | 422µs |
| 10000 | 9994 | 814µs | 728µs | 1.51ms | 2.08ms | 40µs | 375µs |
| 20000 | 19899 | 791µs | 702µs | 1.48ms | 2.25ms | 29µs | 338µs |
| 30000 | 29718 | 1.19ms | 917µs | 2.34ms | 4.64ms | 32µs | 427µs |
| 40000 | 39366 | 1.99ms | 1.26ms | 4.75ms | 15.44ms | 69µs | 644µs |
| 50000 | 48673 | 5.21ms | 2.41ms | 19.97ms | 46.49ms | 109µs | 1.05ms |
| 60000 | 57222 | 32.87ms | 44.38ms | 65.82ms | 101.19ms | 1.24ms | 1.78ms |

**Knee detected at 60000 RPS** (latency_above_50ms): p99 101189µs (101.2ms) exceeds 50ms SLA ceiling. Recommended ceiling: **50000 RPS** on this host.

### Interpretation

**~2028 RPS / core** across 24 logical cores at the recommended ceiling. Gateway self-time mean is **109µs** at the recommended ceiling — this is the per-request overhead the gateway adds on top of whatever the upstream takes.

### Where time + allocs go (50000 RPS, 20s CPU window)

**Top CPU (cumulative):**

```
File: gateway
Build ID: f3cdb232e35c0156b99037131c8136997517ec51
Type: cpu
Time: 2026-05-11 16:42:32 PDT
Duration: 20s, Total samples = 167.88s (839.39%)
Showing nodes accounting for 104.25s, 62.10% of 167.88s total
Dropped 1190 nodes (cum <= 0.84s)
      flat  flat%   sum%        cum   cum%
     0.55s  0.33%  0.33%    106.49s 63.43%  net/http.(*conn).serve
     0.08s 0.048%  0.38%     64.80s 38.60%  net/http.serverHandler.ServeHTTP
     0.01s 0.006%  0.38%     64.72s 38.55%  net/http.(*ServeMux).ServeHTTP
     0.06s 0.036%  0.42%     63.91s 38.07%  net/http.HandlerFunc.ServeHTTP
     0.15s 0.089%  0.51%     63.85s 38.03%  github.com/iodesystems/gwag/gw.(*Gateway).Handler.func1
     0.25s  0.15%  0.66%     61.31s 36.52%  github.com/iodesystems/gwag/gw.(*Gateway).serveGraphQLJSON
     0.17s   0.1%  0.76%     51.37s 30.60%  github.com/graphql-go/graphql.ExecutePlanAppend
     0.10s  0.06%  0.82%     51.09s 30.43%  github.com/graphql-go/graphql.ExecutePlanAppend.func1
     0.63s  0.38%  1.19%     50.97s 30.36%  github.com/graphql-go/graphql.writePlannedSelection
     1.36s  0.81%  2.00%     50.71s 30.21%  github.com/graphql-go/graphql.writePlannedField
```

**Top allocs (cumulative alloc_space):**

```
File: gateway
Build ID: f3cdb232e35c0156b99037131c8136997517ec51
Type: alloc_space
Time: 2026-05-11 16:42:52 PDT
Showing nodes accounting for 107890.04MB, 90.62% of 119063.34MB total
Dropped 668 nodes (cum <= 595.32MB)
      flat  flat%   sum%        cum   cum%
  146.50MB  0.12%  0.12% 102757.90MB 86.31%  net/http.(*conn).serve
         0     0%  0.12% 87714.35MB 73.67%  net/http.(*ServeMux).ServeHTTP
         0     0%  0.12% 87714.35MB 73.67%  net/http.HandlerFunc.ServeHTTP
         0     0%  0.12% 87714.35MB 73.67%  net/http.serverHandler.ServeHTTP
         0     0%  0.12% 87506.17MB 73.50%  github.com/iodesystems/gwag/gw.(*Gateway).Handler.func1
         0     0%  0.12% 85155.07MB 71.52%  github.com/iodesystems/gwag/gw.(*Gateway).serveGraphQLJSON
         0     0%  0.12% 67275.80MB 56.50%  github.com/graphql-go/graphql.ExecutePlanAppend
         0     0%  0.12% 66832.28MB 56.13%  github.com/graphql-go/graphql.ExecutePlanAppend.func1
  438.52MB  0.37%  0.49% 66832.28MB 56.13%  github.com/graphql-go/graphql.writePlannedSelection
 1586.58MB  1.33%  1.82% 66393.76MB 55.76%  github.com/graphql-go/graphql.writePlannedField
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
