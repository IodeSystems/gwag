# perf/ — competitor comparison harness

Containerised head-to-head perf: gwag vs Apollo Router vs
graphql-mesh vs WunderGraph against the same `hello-proto`,
`hello-openapi`, `hello-graphql` backends.

`docs/perf.md` is the on-your-hardware sweep. This directory is the
side-by-side. Each gateway runs its own sweep in turn (serial,
single-process). Result lands at `perf/comparison.md`.

## Layout

```
perf/
  Dockerfile               ubuntu:latest + Go + Node + each gateway binary
  competitors.yaml         declarative list of gateways to test
  cmd/compare/             Go orchestrator (boots backends, sweeps each gateway)
  configs/
    mesh/                  graphql-mesh config
    apollo/                Apollo Router config
    wundergraph/           WunderGraph config (TODO)
  run.sh                   container entrypoint
```

Status: gwag and graphql-mesh wired; Apollo Router in single-subgraph
mode against `hello-graphql`; WunderGraph not yet integrated. Current
numbers: [`comparison.md`](./comparison.md).

## How to run

Docker (hermetic):
```bash
docker build -t gwag-perf -f perf/Dockerfile .
docker run --rm \
  -v $(pwd)/perf/.out:/out \
  -e HOST_UID=$(id -u) -e HOST_GID=$(id -g) \
  gwag-perf
```

Host-local (faster iteration; needs the bench stack down):
```bash
bin/bench down                       # frees :18080 + :50090
perf/run.sh local
perf/run.sh local --only gwag        # debug: one gateway
```

## Caveats

- **Apollo Router and WunderGraph are GraphQL-federation specialists.**
  They appear here in single-subgraph mode (forwarding to
  `hello-graphql`) — the closest apples-to-apples row for a
  GraphQL-in / GraphQL-out cost.
- **graphql-mesh is the closest peer** on multi-format ingest.
- **Numbers reflect this host's hardware**, not absolute speed.
  Same driver hits every gateway, so the relative ordering carries
  even if the absolute RPS doesn't.
- **First Docker build pulls ~1 GB** (Go + Node + Apollo binary +
  npm tree); allow 5–10 minutes. If `graphql-mesh` or `Apollo
  Router` versions drift, pin in `configs/mesh/package.json` or
  bump `APOLLO_VERSION` in `scripts/start-apollo.sh` + `Dockerfile`.
