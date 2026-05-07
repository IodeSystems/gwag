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

## Boot

```
bench/up.sh
```

Builds `gateway`, `greeter`, and `traffic` binaries into
`bench/.run/bin/`, starts one gateway (`n1` on `:18080`) plus one
greeter backend (`g1` registered through the control plane), then
brings up Prometheus on `:19090` and Grafana on `:3001`.

URLs printed at the end include this box's LAN IP — your other
machine can hit `http://<lan-ip>:3001` for Grafana.

## Scaling

Everything dynamic goes through `bench/scale.sh`:

```
bench/scale.sh status                          # what's running
bench/scale.sh add-gateway                     # next-free n2/n3/...
bench/scale.sh add-backend greeter --version v2
bench/scale.sh add-backend greeter --gateway n2
bench/scale.sh rm n2
bench/scale.sh rm g3
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

```
bench/.run/bin/traffic \
  --target http://localhost:18080/api/graphql \
  --target http://localhost:18081/api/graphql \
  --rps 500 --duration 30s --concurrency 32
```

- `--rps` is per-target. Two targets at `--rps 500` is 1k/s total.
- `--concurrency` caps simultaneous in-flight per target. When the
  ticker would fire while saturated, the request is dropped and
  counted as an error so it shows up in the summary instead of
  silently waiting.
- `--query` overrides the default greeter query. Use it to exercise
  other shapes (mutations, deeper subselections, multi-version
  fields, etc.).

The summary prints per-target count / error rate / p50 / p95 / p99 /
max. Use the Grafana dashboard for the gateway-side view (queue
depth, dispatch quantiles, backoff rate, etc.).

## Tear down

```
bench/down.sh           # kill processes + docker compose down
bench/down.sh --purge   # also wipe .run/ (binaries, NATS data, logs)
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
- Only `greeter` (gRPC) backends are wired up. OpenAPI and
  downstream-GraphQL benchmark backends are noted in `docs/plan.md`
  as a follow-up.
- Removing the only gateway tears down the JetStream registry. For
  failover testing add at least one extra gateway first.
