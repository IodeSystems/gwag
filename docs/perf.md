<!--
DO NOT EDIT — this file regenerates from sweep JSONs via 'bin/bench perf report'.
Run 'bin/bench perf all' to refresh the inputs under bench/.run/perf/.
-->

# Performance

> _Generated 2026-05-11T23:09:37Z from 1 scenario sweep via `bin/bench perf report`._

**Headline (proto scenario, last healthy rung):** **70993 RPS** at p95 **85.41ms** with gateway self-time mean **20.73ms**.

## Machine

| Field | Value |
|---|---|
| Captured at | 2026-05-11T23:07:20Z |
| CPU | AMD Ryzen 9 3900X 12-Core Processor |
| Cores (logical) | 24 |
| RAM | 125.7 GiB |
| OS | Ubuntu 24.04 |
| Kernel | 6.8.0-111-generic |
| Arch | amd64 |
| Go | go1.26.2 |
| Gateway rev | f022f5e (dirty) |


## Scenario: `proto`
pure proto/gRPC backend (greeter); baseline for native-format dispatch cost.


- Endpoint: `http://localhost:18080/api/graphql`
- Duration per rep: `5.0s` × 3 reps (rep 1 discarded as warm-up)

| Target RPS | Achieved | Client mean | p50 | p95 | p99 | Gateway self (mean) | Dispatch (mean) |
|---:|---:|---:|---:|---:|---:|---:|---:|
| 1000 | 1000 | 535µs | 537µs | 622µs | 776µs | 42µs | 279µs |
| 5000 | 4997 | 637µs | 611µs | 898µs | 1.27ms | 44µs | 334µs |
| 10000 | 9995 | 746µs | 675µs | 1.38ms | 1.92ms | 37µs | 340µs |
| 20000 | 19879 | 838µs | 723µs | 1.55ms | 2.66ms | 29µs | 336µs |
| 30000 | 29769 | 1.26ms | 966µs | 2.71ms | 5.19ms | 32µs | 461µs |
| 40000 | 39654 | 2.02ms | 1.40ms | 5.03ms | 11.55ms | 68µs | 741µs |
| 50000 | 48788 | 7.23ms | 4.41ms | 26.58ms | 43.84ms | 490µs | 1.75ms |
| 60000 | 57103 | 35.42ms | 43.53ms | 64.58ms | 95.44ms | 7.40ms | 2.80ms |
| 75000 | 70993 | 50.21ms | 56.87ms | 85.41ms | 118.45ms | 20.73ms | 3.33ms |

No knee detected within the configured sweep — the gateway absorbed every rung tested without tripping the achieved-below-80%-of-target or p99-doubled predicates. Push higher with `--steps` to find the actual ceiling.

### Interpretation

**~2958 RPS / core** across 24 logical cores at the recommended ceiling. Gateway self-time mean is **20.73ms** at the recommended ceiling — this is the per-request overhead the gateway adds on top of whatever the upstream takes.


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
