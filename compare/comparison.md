# Perf comparison — gwag vs peers

_Generated 2026-08-17T20:57:24Z. Run via `docker run gwag-compare` or `compare/run.sh local`._

Each gateway runs the same `bench/cmd/traffic` sweep against the same `hello-*` backends on the same host (serial; no concurrent gateways). Knee = highest rung where p99 stays under 50ms.

## Headline matrix

| Scenario | gwag | gat | mesh | apollo | 
|---|---|---|---|---|
| **proto** | 40000 RPS @ p99 31.7ms | 40000 RPS @ p99 30.4ms | not supported | not supported | 
| **openapi** | 50000 RPS @ p99 43.5ms | 40000 RPS @ p99 17.2ms | 20000 RPS @ p99 49.5ms | not supported | 
| **graphql** | 40000 RPS @ p99 18.4ms | not supported | 20000 RPS @ p99 38.5ms | 20000 RPS @ p99 39.6ms | 

## gwag

this repo — multi-format ingest, dynamic registration, reflection dispatch

| Scenario | Ceiling RPS | Achieved | p99 @ ceiling | Gateway self-time |
|---|---:|---:|---:|---:|
| proto | 40000 | 39332 | 31.7ms | 146µs |
| openapi | 50000 | 47584 | 43.5ms | 541µs |
| graphql | 40000 | 39359 | 18.4ms | 104µs |

## gat

this repo, embedded — gat fronting the same upstreams in-process; no NATS, no cluster, no admin

| Scenario | Ceiling RPS | Achieved | p99 @ ceiling | Gateway self-time |
|---|---:|---:|---:|---:|
| proto | 40000 | 39140 | 30.4ms | 335µs |
| openapi | 40000 | 39376 | 17.2ms | 91µs |

## mesh

graphql-mesh (Node, npm-distributed; multi-format ingest peer)

| Scenario | Ceiling RPS | Achieved | p99 @ ceiling | Gateway self-time |
|---|---:|---:|---:|---:|
| openapi | 20000 | 16866 | 49.5ms | 0µs |
| graphql | 20000 | 19059 | 38.5ms | 0µs |

## apollo

Apollo Router (Rust, GraphQL-federation specialist; single-subgraph mode here)

| Scenario | Ceiling RPS | Achieved | p99 @ ceiling | Gateway self-time |
|---|---:|---:|---:|---:|
| graphql | 20000 | 19628 | 39.6ms | 0µs |

