# Perf comparison — gwag vs peers

_Generated 2026-05-13T10:48:37Z. Run via `docker run gwag-perf` or `perf/run.sh local`._

Each gateway runs the same `bench/cmd/traffic` sweep against the same `hello-*` backends on the same host (serial; no concurrent gateways). Knee = highest rung where p99 stays under 50ms.

## Headline matrix

| Scenario | gwag | mesh | apollo | 
|---|---|---|---|
| **proto** | 40000 RPS @ p99 36.9ms | not supported | not supported | 
| **openapi** | 50000 RPS @ p99 40.9ms | 10000 RPS @ p99 49.8ms | not supported | 
| **graphql** | 40000 RPS @ p99 14.2ms | 10000 RPS @ p99 16.9ms | 10000 RPS @ p99 34.2ms | 

## gwag

this repo — multi-format ingest, dynamic registration, reflection dispatch

| Scenario | Ceiling RPS | Achieved | p99 @ ceiling | Gateway self-time |
|---|---:|---:|---:|---:|
| proto | 40000 | 39014 | 36.9ms | 475µs |
| openapi | 50000 | 48178 | 40.9ms | 222µs |
| graphql | 40000 | 39371 | 14.2ms | 64µs |

## mesh

graphql-mesh (Node, npm-distributed; multi-format ingest peer)

| Scenario | Ceiling RPS | Achieved | p99 @ ceiling | Gateway self-time |
|---|---:|---:|---:|---:|
| openapi | 10000 | 9337 | 49.8ms | 0µs |
| graphql | 10000 | 9732 | 16.9ms | 0µs |

## apollo

Apollo Router (Rust, GraphQL-federation specialist; single-subgraph mode here)

| Scenario | Ceiling RPS | Achieved | p99 @ ceiling | Gateway self-time |
|---|---:|---:|---:|---:|
| graphql | 10000 | 9518 | 34.2ms | 0µs |

