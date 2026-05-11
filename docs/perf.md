<!--
DO NOT EDIT — this file regenerates from sweep JSONs via 'bin/bench perf report'.
Run 'bin/bench perf all' to refresh the inputs under bench/.run/perf/.
-->

# Performance

> _Generated 2026-05-11T17:04:03Z from 1 scenario sweep via `bin/bench perf report`._

**Headline (proto scenario, last healthy rung):** **1873 RPS** at p95 **633µs** with gateway self-time mean **70µs**.

## Machine

| Field | Value |
|---|---|
| Captured at | 2026-05-11T17:03:00Z |
| CPU | AMD Ryzen 9 3900X 12-Core Processor |
| Cores (logical) | 24 |
| RAM | 125.7 GiB |
| OS | Ubuntu 24.04 |
| Kernel | 6.8.0-111-generic |
| Arch | amd64 |
| Go | go1.26.2 |
| Gateway rev | aa443da |


## Scenario: `proto`
pure proto/gRPC backend (greeter); baseline for native-format dispatch cost.


- Endpoint: `http://localhost:18080/api/graphql`
- Duration per rep: `5.0s` × 3 reps (rep 1 discarded as warm-up)

| Target RPS | Achieved | Client mean | p50 | p95 | p99 | Gateway self (mean) | Dispatch (mean) |
|---:|---:|---:|---:|---:|---:|---:|---:|
| 500 | 500 | 515µs | 516µs | 583µs | 759µs | 71µs | 262µs |
| 1000 | 950 | 525µs | 520µs | 642µs | 829µs | 71µs | 270µs |
| 2000 | 1873 | 518µs | 510µs | 633µs | 875µs | 70µs | 266µs |
| 5000 | 3745 | 506µs | 495µs | 786µs | 1.10ms | 64µs | 251µs |

**Knee detected at 5000 RPS** (achieved_below_80pct): achieved 3745 / 5000 target (75% < 80% threshold). Recommended ceiling: **2000 RPS** on this host.

### Interpretation

**~78 RPS / core** across 24 logical cores at the recommended ceiling. Gateway self-time mean is **70µs** at the recommended ceiling — this is the per-request overhead the gateway adds on top of whatever the upstream takes. The knee fired because achieved RPS fell below 80% of target — typically the bench client itself running out of fired RPS, the gateway, or an upstream cap. Drill into `bench/.run/perf/sweep-proto.reps/` with `--keep-reps` to see which.


## How to read this

Three numbers tell most of the story per scenario:

- **Achieved RPS / target RPS** — anything < 80 % of target is saturation (gateway, client, or upstream).
- **Gateway self (mean)** — the gateway-only slice of each request (`request_self_seconds` mean). Compare across upstream-latency runs to see "how much does the gateway add"; this number should be roughly upstream-independent.
- **Dispatch (mean)** — upstream time as measured by the gateway. Climbs with configured upstream latency; the delta vs. self-time is the upstream's contribution.

### Knee heuristic

A rung is flagged as the knee when:

- **achieved_below_80pct** — actual RPS < 0.80 × target. The client / gateway / upstream couldn't keep up.
- **p99_doubled** — step's p99 > 2 × prior step's p99. Latency cliff before throughput cliff.

First-firing predicate stops the sweep; the prior step is the recommended ceiling. Pass `--no-knee` to `bench perf run` to walk every rung regardless (useful for the full curve).

### Regenerating

```bash
# 1. Bring up the stack and the upstream services each scenario needs.
bin/bench up
bin/bench service add greeter          # proto scenario needs greeter
# bin/bench service add greeter --delay 100us   # for the upstream-latency rungs

# 2. Run sweeps (one per scenario).
bin/bench perf all --out-dir bench/.run/perf

# 3. Render this file.
bin/bench perf report --in-dir bench/.run/perf --out docs/perf.md
```
