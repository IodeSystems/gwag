# Perf comparison — gwag vs peers

_Generated 2026-11-07T03:05:36Z. Run via `docker run gwag-perf` or `compare/run.sh local`._

Each gateway runs the same `bench/cmd/traffic` sweep against the same `hello-*` backends on the same host (serial; no concurrent gateways). Knee = highest rung where p99 stays under 50ms.

## Provenance

- **Host:** Neoverse-V1, 2 threads, Ubuntu 24.04.4 LTS, kernel 7.0.0-1012-aws
- **Toolchains:** go1.27.3 · node v24.12.0
- **Sweep:** rungs [250 500 1000 2000 3000 5000 7500 10000 15000 20000 30000 40000 50000 60000] RPS, 5s per rep, 3 reps (rep 1 discarded as warm-up)
- **Versions:** gwag `v2.11.0-2-g84e7377fac59`, gat `v2.11.0-2-g84e7377fac59`, hive `2.15.3`, mesh `0.100.52`, apollo `2.19.0`, cosmo `0.363.0`, grafbase `0.53.5`

## Headline matrix

| Scenario | gwag | gat | hive | mesh | apollo | cosmo | grafbase | 
|---|---|---|---|---|---|---|---|
| **proto** | 7500 RPS @ p99 30.4ms | 10000 RPS @ p99 21.9ms | 500 RPS @ p99 10.2ms | not supported | not supported | not supported | not supported | 
| **openapi** | 5000 RPS @ p99 19.5ms | 7500 RPS @ p99 39.5ms | 500 RPS @ p99 21.2ms | 1000 RPS @ p99 43.7ms | not supported | not supported | not supported | 
| **graphql** | 5000 RPS @ p99 49.8ms | not supported | 500 RPS @ p99 8.0ms | 500 RPS @ p99 10.7ms | 3000 RPS @ p99 23.7ms | 10000 RPS @ p99 35.8ms | 2000 RPS @ p99 7.6ms | 

## gwag

this repo — multi-format ingest, dynamic registration, reflection dispatch

| Scenario | Ceiling RPS | Achieved | p99 @ ceiling | Gateway self-time |
|---|---:|---:|---:|---:|
| proto | 7500 | 7329 | 30.4ms | 42µs |
| openapi | 5000 | 4940 | 19.5ms | 50µs |
| graphql | 5000 | 4865 | 49.8ms | 40µs |

## gat

this repo, embedded — gat fronting the same upstreams in-process; no NATS, no cluster, no admin

| Scenario | Ceiling RPS | Achieved | p99 @ ceiling | Gateway self-time |
|---|---:|---:|---:|---:|
| proto | 10000 | 9597 | 21.9ms | — |
| openapi | 7500 | 7303 | 39.5ms | — |

## hive

Hive Gateway (Node; Mesh v1 compose-cli supergraph, one worker per core; multi-format ingest peer)

| Scenario | Ceiling RPS | Achieved | p99 @ ceiling | Gateway self-time |
|---|---:|---:|---:|---:|
| proto | 500 | 492 | 10.2ms | — |
| openapi | 500 | 493 | 21.2ms | — |
| graphql | 500 | 495 | 8.0ms | — |

## mesh

graphql-mesh v0 (legacy; Node, npm-distributed; multi-format ingest peer)

| Scenario | Ceiling RPS | Achieved | p99 @ ceiling | Gateway self-time |
|---|---:|---:|---:|---:|
| openapi | 1000 | 975 | 43.7ms | — |
| graphql | 500 | 495 | 10.7ms | — |

## apollo

Apollo Router (Rust, GraphQL-federation specialist; single-subgraph mode here)

| Scenario | Ceiling RPS | Achieved | p99 @ ceiling | Gateway self-time |
|---|---:|---:|---:|---:|
| graphql | 3000 | 2948 | 23.7ms | — |

## cosmo

WunderGraph Cosmo Router (Go, GraphQL-federation specialist; static execution config, single-subgraph mode here)

| Scenario | Ceiling RPS | Achieved | p99 @ ceiling | Gateway self-time |
|---|---:|---:|---:|---:|
| graphql | 10000 | 9539 | 35.8ms | — |

## grafbase

Grafbase Gateway (Rust, GraphQL-federation specialist; static schema, single-subgraph mode here)

| Scenario | Ceiling RPS | Achieved | p99 @ ceiling | Gateway self-time |
|---|---:|---:|---:|---:|
| graphql | 2000 | 1992 | 7.6ms | — |

