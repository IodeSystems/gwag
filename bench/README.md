# bench

Local benchmark + demo stack: a gateway cluster you can scale at
runtime, Prometheus + Grafana scraping `/api/metrics`, and a
traffic generator that hammers the GraphQL surface and reports
client-side latency quantiles.

## Prereqs

- Go (whatever the repo's toolchain wants).
- `docker` + `docker compose`.
- Linux host networking (the compose file uses `network_mode: host`
  so Prometheus can scrape `localhost:1808N` directly).

The bench targets `/api/graphql` and `/api/metrics` only — the admin
SPA at `/` is irrelevant. `ui/dist/index.html` is committed as a
placeholder so `go build` succeeds without a UI build. If you want
the admin panels (services list, peers, schema viewer, events tray)
to render when poking around in a browser, build it once:

```
cd ui && pnpm install && pnpm run build
```

## Entry point: `bin/bench`

One dispatcher for everything:

```
bin/bench up [--build]      boot the stack; --build runs bin/build first
bin/bench restart [--build] down (purging) then up
bin/bench down [--no-purge] kill + `docker compose down`; wipes .run/ unless --no-purge
bin/bench status            list running gateways/backends
bin/bench scale args...     forwards to scale.sh:
                              add-gateway, add-backend, rm, status
bin/bench traffic args...   run the traffic generator (builds it on demand)
bin/bench logs NAME         tail bench/.run/logs/<NAME>.log
```

`bin/bench up` builds `gateway`, `greeter`, and `traffic` binaries
into `bench/.run/bin/`, starts one gateway (`n1` on `:18080`) plus
one greeter backend (`g1` registered through the control plane),
then brings up Prometheus on `:19090` and Grafana on `:3001`. URLs
printed at the end include this box's LAN IP — your other machine
can hit `http://<lan-ip>:3001` for Grafana.

## Scaling at runtime

```
bin/bench status
bin/bench scale add-gateway                    # next-free n2/n3/...
bin/bench scale add-backend greeter --version v2
bin/bench scale add-backend greeter --gateway n2
bin/bench scale rm n2
bin/bench scale rm g3
```

Adding a gateway joins the existing NATS cluster (the new node's
`--nats-peer` is set to every live cluster's NATS-cluster port), so
the JetStream registry stays consistent and dispatch from any node
reaches replicas registered against any other.

Adding a backend self-registers via the control plane on the chosen
gateway (or any live one if you don't pick). Replicas of the same
`(namespace, version)` get added to the existing pool.

`add-gateway` also rewrites `bench/.run/targets.json` — Prometheus
file-SD picks up the new scrape target within ~10s without a reload.

## Traffic

The traffic generator picks one of three subcommands by ingress
format. All three target the same registered service through the
gateway's IR-translation layer — apples-to-apples per-format cost.

```
# GraphQL — POST a query to /api/graphql
bin/bench traffic graphql \
  --target http://localhost:18080/api/graphql \
  --rps 500 --duration 30s

# gRPC — unary RPC via gw.GRPCUnknownHandler on the control-plane port
bin/bench traffic grpc \
  --target http://localhost:18080 \
  --grpc-target localhost:50090 \
  --service greeter --method Hello \
  --args '{"name":"world"}' \
  --rps 500 --duration 30s

# OpenAPI — HTTP/JSON via gw.IngressHandler at /api/ingress/...
bin/bench traffic openapi \
  --target http://localhost:18080 \
  --service greeter --operation Hello \
  --args '{"name":"world"}' \
  --rps 500 --duration 30s
```

Shared flags across all three: `--rps`, `--duration`, `--concurrency`,
`--timeout`, `--target`, `--server-metrics`. Run
`traffic <sub> --help` for per-subcommand flag lists.

- `--target` is repeatable / comma-separable for multi-gateway runs;
  `--rps` is per-target.
- `--concurrency` caps simultaneous in-flight per target. Default is
  `0` = auto = `max(64, rps/20)`, scaling the headroom with load so
  the bench client doesn't silently cap throughput. Saturation drops
  are counted as errors (`drop` category) so a too-low cap shows up
  in the summary, and the runner prints a Little's-law advisor at
  end-of-run when `concurrency × p50 < target rps`.
- `traffic graphql --query '{...}'` overrides the default greeter query.
- `traffic grpc/openapi --args '{...}'` provides the request payload;
  resolved against the gateway-rendered FDS (`/api/schema/proto`) or
  spec (`/api/schema/openapi`). The openapi adapter honours the spec
  verbatim: declared path/query params are extracted from `--args`,
  remaining args land in the JSON body when the op declares a
  requestBody (proto-unary unary args now synthesise as a body schema,
  matching `IngressHandler`'s ingressShapeProtoPost decode).

Summary blocks: per-target row with RPS / P50 / P95 / P99 / OK /
ERRS / CODES, plus example response bodies per status code (so a
4xx body is right there, not buried in Grafana). Gateway-side block
follows: per-(namespace, version, method) RPS / P50 / P95 / P99 /
COUNT / CODES from the gateway's own histograms (server view; lower
bound per bucket means short requests look pessimistic — use the
client row for sub-millisecond precision). Below that, a per-ingress
request-time row pulls `request_duration_seconds` and
`request_self_seconds` (mean + p95) so operators can see "client p50
= 3.5 ms; gateway self = 0.8 ms; greeter dispatch = 2.7 ms" at a
glance — the answer to "is this on us?" without writing PromQL.

## Tear down

```
bin/bench down              # purges .run/ by default
bin/bench down --no-purge   # keep .run/ for a faster re-up
```

## Layout

```
bench/
  docker-compose.yml         prom + grafana, host network
  prometheus.yml             scrape config, file_sd → .run/targets.json
  grafana/
    provisioning/            datasource + dashboard provider YAMLs
    dashboards/gateway.json  starter panels
  cmd/traffic/main.go        the load generator
  lib.sh                     shared bash helpers
  up.sh / down.sh / scale.sh
  .run/                      runtime state (gitignored)
    bin/                     built binaries
    gateways/<name>.env      one per gateway: ports, pid
    backends/<name>.env      one per backend: port, kind, version, gateway, pid
    nats/<name>/             per-gateway JetStream data dir
    logs/<name>.log          stdout+stderr per process
    targets.json             prometheus file_sd
```

## Known limits

- Single host only. Multi-host benchmarking (real network latency,
  separate containers) is a future follow-up — none of this requires
  k8s, but the orchestrator scripts assume `localhost`.
- Only `greeter` (gRPC-registered) backends are wired up by the
  scale scripts. The three traffic adapters can all hit greeter via
  different ingress formats (graphql / grpc / openapi) thanks to the
  IR translation, so format comparisons work today. An OpenAPI- or
  downstream-GraphQL-registered demo backend would round out per-source
  perf comparisons but isn't needed for the (ingress × source) matrix
  to pass — that's covered by `gw/cross_format_ingress_test.go`.
- Removing the only gateway tears down the JetStream registry. For
  failover testing add at least one extra gateway first.
