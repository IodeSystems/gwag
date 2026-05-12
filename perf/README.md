# perf/ — competitor comparison harness

Containerised head-to-head perf comparison: gwag vs Apollo Router vs
graphql-mesh vs WunderGraph against the same `hello-proto`,
`hello-openapi`, `hello-graphql` backends.

## Why this exists

`docs/perf.md` answers _"how does gwag perform on my hardware?"_.
This directory answers _"how does gwag stack up against the obvious
peers when they all run the same workload on the same box?"_.

Result lands at `perf/comparison.md`. Each gateway runs its own
sweep in turn (serial, single-process) so they're never competing
for CPU.

## Layout

```
perf/
  Dockerfile               ubuntu:latest + Go + Node + each gateway binary
  competitors.yaml         declarative list of gateways to test
  cmd/compare/             Go orchestrator (boots backends, sweeps each gateway)
  configs/
    mesh/                  graphql-mesh config (npm-distributed peer)
    apollo/                Apollo Router config (Rust binary peer)
    wundergraph/           WunderGraph config (TODO — codegen-heavy)
  run.sh                   container entrypoint
```

## What's implemented

| Gateway | Status | Notes |
|---|---|---|
| gwag (us) | ✅ working | reuses `bench/cmd/perf` driver + scenarios |
| graphql-mesh | 🟡 scaffolded | npm-installed; config wires hello-graphql + hello-openapi |
| Apollo Router | 🟡 scaffolded | single-subgraph "federation" against hello-graphql only |
| WunderGraph | ❌ TODO | codegen + dual-process; harder integration; deferred |

## How to run

**Docker (recommended — full hermetic env):**
```bash
# perf/docker-build.sh stages the graphql-go fork into perf/.build/graphql
# (go.mod has a host-absolute replace directive that Docker can't see)
# then runs `docker build`.
perf/docker-build.sh

# Run the full comparison sweep (writes perf/.out/comparison.md):
docker run --rm -v $(pwd)/perf/.out:/out gwag-perf
```

**Host-local (faster iteration, requires bench stack down):**
```bash
# Tear down bench/.run/ first — port :18080 + :50090 conflict.
bin/bench down

# Run from the repo root (the orchestrator binary lives at perf/.run/bin/compare):
perf/run.sh local
```

**Restricting to one gateway** (debug):
```bash
perf/run.sh local --only gwag
perf/run.sh local --only mesh
```

## Caveats / current state

**This is a sketch you'll likely need to debug.** First Docker build
will pull ~1GB (Go toolchain + Node + Apollo binary + npm tree) and
take 5-10 minutes. Common first-iteration problems:

- `graphql-mesh` npm install may need version pins adjusted in
  `configs/mesh/package.json` as the upstream packages move
- `Apollo Router` config may reject the hand-written supergraph SDL
  if the runtime version expects different `@link`/`@join__*`
  versions; bump `APOLLO_VERSION` in `scripts/start-apollo.sh` +
  `Dockerfile` to match
- `compare`'s `waitForGateway` polls /endpoint with a `__typename`
  query, which works for most GraphQL stacks but might need tweaking
  for Apollo Router if it returns 4xx on probe queries

## Caveats

- **Apollo Router and WunderGraph are GraphQL-federation specialists.**
  Apples-to-apples comparison against the full proto/openapi/graphql
  matrix needs them to consume non-GraphQL backends, which they
  don't natively do. They appear here in single-subgraph mode (just
  forwarding to `hello-graphql`) to give a comparable
  "GraphQL-in / GraphQL-out gateway" cost number.
- **graphql-mesh is the closest peer** — same problem shape
  (multi-format ingest → unified GraphQL surface). The most
  apples-to-apples row.
- **Numbers reflect this host's hardware**, not absolute speed.
  Same `bench/cmd/traffic` driver hits every gateway, so the
  *relative* ordering is meaningful even if the absolute RPS isn't
  portable.
