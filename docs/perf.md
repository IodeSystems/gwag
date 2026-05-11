<!--
DO NOT EDIT — this file regenerates from sweep JSONs via 'bin/bench perf report'.
Run 'bin/bench perf all' to refresh the inputs under bench/.run/perf/.
-->

# Performance

> _Generated 2026-05-11T18:00:16Z from 1 scenario sweep via `bin/bench perf report`._

**Headline (proto scenario, last healthy rung):** **24718 RPS** at p95 **10.15ms** with gateway self-time mean **51µs**.

## Machine

| Field | Value |
|---|---|
| Captured at | 2026-05-11T17:58:45Z |
| CPU | AMD Ryzen 9 3900X 12-Core Processor |
| Cores (logical) | 24 |
| RAM | 125.7 GiB |
| OS | Ubuntu 24.04 |
| Kernel | 6.8.0-111-generic |
| Arch | amd64 |
| Go | go1.26.2 |
| Gateway rev | c095b69 (dirty) |


## Scenario: `proto`
pure proto/gRPC backend (greeter); baseline for native-format dispatch cost.


- Endpoint: `http://localhost:18080/api/graphql`
- Duration per rep: `5.0s` × 3 reps (rep 1 discarded as warm-up)

| Target RPS | Achieved | Client mean | p50 | p95 | p99 | Gateway self (mean) | Dispatch (mean) |
|---:|---:|---:|---:|---:|---:|---:|---:|
| 1000 | 987 | 496µs | 481µs | 603µs | 858µs | 41µs | 269µs |
| 5000 | 4154 | 601µs | 561µs | 943µs | 1.23ms | 44µs | 258µs |
| 10000 | 8187 | 559µs | 523µs | 978µs | 1.34ms | 34µs | 216µs |
| 20000 | 19696 | 1.31ms | 1.06ms | 2.83ms | 4.27ms | 38µs | 400µs |
| 30000 | 24719 | 3.41ms | 1.90ms | 10.15ms | 36.30ms | 51µs | 1.07ms |
| 40000 | 25510 | 9.37ms | 4.29ms | 39.47ms | 99.07ms | 339µs | 2.79ms |

**Knee detected at 40000 RPS** (achieved_below_80pct): achieved 25510 / 40000 target (64% < 80% threshold). Recommended ceiling: **30000 RPS** on this host.

### Interpretation

**~1030 RPS / core** across 24 logical cores at the recommended ceiling. Gateway self-time mean is **51µs** at the recommended ceiling — this is the per-request overhead the gateway adds on top of whatever the upstream takes. The knee fired because achieved RPS fell below 80% of target — typically the bench client itself running out of fired RPS, the gateway, or an upstream cap. Drill into `bench/.run/perf/sweep-proto.reps/` with `--keep-reps` to see which.


## How to read this

Three numbers tell most of the story per scenario:

- **Achieved RPS / target RPS** — anything < 80 % of target is saturation (gateway, client, or upstream).
- **Gateway self (mean)** — the gateway-only slice of each request (`request_self_seconds` mean). Compare across upstream-latency runs to see "how much does the gateway add"; this number should be roughly upstream-independent.
- **Dispatch (mean)** — upstream time as measured by the gateway. Climbs with configured upstream latency; the delta vs. self-time is the upstream's contribution.

### Knee heuristic

A rung is flagged as the knee when:

- **achieved_below_80pct** — actual RPS < 0.80 × target. The client / gateway / upstream couldn't keep up; throughput collapsed.
- **p99_cliff** — step's p99 > 2 × prior step's p99 **AND** achieved RPS no longer climbed vs the prior step. Catches saturation-via-latency: the gateway is going slow rather than dropping requests. A pure latency creep with healthy throughput growth is normal queueing, not a knee, and is intentionally not flagged.

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
