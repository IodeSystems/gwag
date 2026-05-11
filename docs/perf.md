<!--
DO NOT EDIT — this file regenerates from sweep JSONs via 'bin/bench perf report'.
Run 'bin/bench perf all' to refresh the inputs under bench/.run/perf/.
-->

# Performance

> _Generated 2026-05-11T20:15:32Z from 1 scenario sweep via `bin/bench perf report`._

**Headline (proto scenario, last healthy rung):** **25306 RPS** at p95 **7.55ms** with gateway self-time mean **64µs**.

## Machine

| Field | Value |
|---|---|
| Captured at | 2026-05-11T20:14:02Z |
| CPU | AMD Ryzen 9 3900X 12-Core Processor |
| Cores (logical) | 24 |
| RAM | 125.7 GiB |
| OS | Ubuntu 24.04 |
| Kernel | 6.8.0-111-generic |
| Arch | amd64 |
| Go | go1.26.2 |
| Gateway rev | 48b7ded (dirty) |


## Scenario: `proto`
pure proto/gRPC backend (greeter); baseline for native-format dispatch cost.


- Endpoint: `http://localhost:18080/api/graphql`
- Duration per rep: `5.0s` × 3 reps (rep 1 discarded as warm-up)

| Target RPS | Achieved | Client mean | p50 | p95 | p99 | Gateway self (mean) | Dispatch (mean) |
|---:|---:|---:|---:|---:|---:|---:|---:|
| 1000 | 969 | 489µs | 481µs | 588µs | 791µs | 39µs | 265µs |
| 5000 | 4052 | 588µs | 544µs | 909µs | 1.24ms | 42µs | 253µs |
| 10000 | 8306 | 562µs | 526µs | 1.00ms | 1.38ms | 34µs | 204µs |
| 20000 | 19333 | 1.66ms | 1.31ms | 3.65ms | 5.47ms | 43µs | 383µs |
| 30000 | 25306 | 3.39ms | 2.58ms | 7.55ms | 19.52ms | 64µs | 789µs |
| 40000 | 26390 | 7.51ms | 4.30ms | 25.61ms | 67.84ms | 220µs | 1.73ms |

**Knee detected at 40000 RPS** (achieved_below_80pct): achieved 26390 / 40000 target (66% < 80% threshold). Recommended ceiling: **30000 RPS** on this host.

### Interpretation

**~1054 RPS / core** across 24 logical cores at the recommended ceiling. Gateway self-time mean is **64µs** at the recommended ceiling — this is the per-request overhead the gateway adds on top of whatever the upstream takes. The knee fired because achieved RPS fell below 80% of target — typically the bench client itself running out of fired RPS, the gateway, or an upstream cap. Drill into `bench/.run/perf/sweep-proto.reps/` with `--keep-reps` to see which.


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
