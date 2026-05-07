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

```
bin/bench traffic \
  --target http://localhost:18080/api/graphql \
  --target http://localhost:18081/api/graphql \
  --rps 500 --duration 30s --concurrency 32
```

- `--rps` is per-target. Two targets at `--rps 500` is 1k/s total.
- `--concurrency` caps simultaneous in-flight per target. Saturation
  drops are counted as errors (`drop` category) so a too-low cap
  shows up in the summary.
- `--query` overrides the default greeter query.

Summary blocks: per-target row with RPS / P50 / P95 / P99 / OK /
ERRS / CODES, plus example response bodies per status code (so a
4xx body is right there, not buried in Grafana). Gateway-side block
follows: per-(namespace, version, method) RPS / P50 / P95 / P99 /
COUNT / CODES from the gateway's own histograms (server view; lower
bound per bucket means short requests look pessimistic — use the
client row for sub-millisecond precision).

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
- Only `greeter` (gRPC) backends are wired up. OpenAPI and
  downstream-GraphQL benchmark backends are noted in `docs/plan.md`
  as a follow-up.
- Removing the only gateway tears down the JetStream registry. For
  failover testing add at least one extra gateway first.
