# Perf comparison — gwag vs peers

_Generated 2026-08-18T02:02:34Z. Run via `docker run gwag-perf` or `compare/run.sh local`._

Each gateway runs the same `bench/cmd/traffic` sweep against the same `hello-*` backends on the same host (serial; no concurrent gateways). Knee = highest rung where p99 stays under 50ms.

## Headline matrix

| Scenario | gwag | gat | mesh | apollo | 
|---|---|---|---|---|
| **proto** | 50000 RPS @ p99 41.4ms | 50000 RPS @ p99 14.6ms | not supported | not supported | 
| **openapi** | 50000 RPS @ p99 45.8ms | 40000 RPS @ p99 12.6ms | 20000 RPS @ p99 47.1ms | not supported | 
| **graphql** | 40000 RPS @ p99 10.3ms | not supported | 20000 RPS @ p99 24.6ms | 20000 RPS @ p99 13.9ms | 

## gwag

this repo — multi-format ingest, dynamic registration, reflection dispatch

| Scenario | Ceiling RPS | Achieved | p99 @ ceiling | Gateway self-time |
|---|---:|---:|---:|---:|
| proto | 50000 | 48878 | 41.4ms | 174µs |
| openapi | 50000 | 48247 | 45.8ms | 184µs |
| graphql | 40000 | 39462 | 10.3ms | 42µs |

## gat

this repo, embedded — gat fronting the same upstreams in-process; no NATS, no cluster, no admin

| Scenario | Ceiling RPS | Achieved | p99 @ ceiling | Gateway self-time |
|---|---:|---:|---:|---:|
| proto | 50000 | 49375 | 14.6ms | 0µs |
| openapi | 40000 | 39500 | 12.6ms | 0µs |

## mesh

graphql-mesh (Node, npm-distributed; multi-format ingest peer)

| Scenario | Ceiling RPS | Achieved | p99 @ ceiling | Gateway self-time |
|---|---:|---:|---:|---:|
| openapi | 20000 | 17628 | 47.1ms | 0µs |
| graphql | 20000 | 19158 | 24.6ms | 0µs |

## apollo

Apollo Router (Rust, GraphQL-federation specialist; single-subgraph mode here)

| Scenario | Ceiling RPS | Achieved | p99 @ ceiling | Gateway self-time |
|---|---:|---:|---:|---:|
| graphql | 20000 | 19828 | 13.9ms | 0µs |

