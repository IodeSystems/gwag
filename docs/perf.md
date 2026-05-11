<!--
DO NOT EDIT — this file regenerates from sweep JSONs via 'bin/bench perf report'.
Run 'bin/bench perf all' to refresh the inputs under bench/.run/perf/.
-->

# Performance

> _Generated 2026-05-11T23:17:50Z from 1 scenario sweep via `bin/bench perf report`._

**Headline (proto scenario, last healthy rung):** **48795 RPS** at p95 **14.79ms** with gateway self-time mean **138µs**.

## Machine

| Field | Value |
|---|---|
| Captured at | 2026-05-11T23:15:49Z |
| CPU | AMD Ryzen 9 3900X 12-Core Processor |
| Cores (logical) | 24 |
| RAM | 125.7 GiB |
| OS | Ubuntu 24.04 |
| Kernel | 6.8.0-111-generic |
| Arch | amd64 |
| Go | go1.26.2 |
| Gateway rev | 2e5594a (dirty) |


## Scenario: `proto`
pure proto/gRPC backend (greeter); baseline for native-format dispatch cost.


- Endpoint: `http://localhost:18080/api/graphql`
- Duration per rep: `5.0s` × 3 reps (rep 1 discarded as warm-up)

| Target RPS | Achieved | Client mean | p50 | p95 | p99 | Gateway self (mean) | Dispatch (mean) |
|---:|---:|---:|---:|---:|---:|---:|---:|
| 1000 | 999 | 526µs | 526µs | 623µs | 812µs | 41µs | 275µs |
| 5000 | 4998 | 643µs | 618µs | 955µs | 1.28ms | 45µs | 326µs |
| 10000 | 9994 | 757µs | 686µs | 1.39ms | 1.85ms | 41µs | 343µs |
| 20000 | 19940 | 801µs | 711µs | 1.46ms | 2.22ms | 32µs | 314µs |
| 30000 | 29747 | 1.14ms | 950µs | 2.23ms | 3.54ms | 36µs | 398µs |
| 40000 | 39677 | 1.53ms | 1.07ms | 3.74ms | 6.88ms | 41µs | 561µs |
| 50000 | 48795 | 5.14ms | 3.21ms | 14.79ms | 35.31ms | 138µs | 1.07ms |
| 60000 | 57048 | 32.47ms | 44.87ms | 63.12ms | 103.53ms | 432µs | 1.65ms |

**Knee detected at 60000 RPS** (latency_above_50ms): p99 103525µs (103.5ms) exceeds 50ms SLA ceiling. Recommended ceiling: **50000 RPS** on this host.

### Interpretation

**~2033 RPS / core** across 24 logical cores at the recommended ceiling. Gateway self-time mean is **138µs** at the recommended ceiling — this is the per-request overhead the gateway adds on top of whatever the upstream takes.


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
