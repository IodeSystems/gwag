# e2e

End-to-end stories: real gateway and service processes, driven over
their real ports. Every file has the `e2e` build tag, so `go build ./...`
and `go test ./gw/...` never compile or run any of it.

```
go test -tags e2e ./e2e/...                      # all stories
go test -tags e2e ./e2e/ -run TestS0 -v          # one story
E2E_LOG_LINES=200 go test -tags e2e ./e2e/...    # longer log tails on failure
E2E_KNOWN_BUGS=1 go test -tags e2e ./e2e/ -run KnownBug   # reproduce open bugs
```

The first test that needs a binary builds it with `go build` into a temp
directory (once per `go test` run, removed afterwards). If `ui/dist/` is
empty, the harness copies `ui/fallback/index.html` into it, the same way
`bin/build` does.

## Layout

- `harness/` — process manager, gateway and cluster start-up, fixture
  start-up, GraphQL / REST / gRPC / Connect / admin / MCP clients,
  `/api/metrics` scraper, load generator, `Eventually`.
- `fixture/` — one upstream service binary. Serves `Echo` as proto, OpenAPI
  or GraphQL; self-registers with `controlclient.SelfRegister`; injects
  faults. `fixture.proto` is compiled at boot (protocompile + dynamicpb),
  so there is no generated code.
- `*_test.go` here — the stories. `known_bugs_test.go` holds tests for
  open gateway bugs; they skip unless `E2E_KNOWN_BUGS=1`.

The gateway is `examples/multi/cmd/gateway`: it has the flags the
stories need (backpressure caps, `--caller-id-public`, NATS cluster,
`--admin-data-dir`) and is what `examples/multi/demo.sh` runs.

## Writing a story

```go
//go:build e2e

package e2e

func TestRollingRelease(t *testing.T) {
	env := harness.New(t)                    // procs die and logs dump at test end
	gw := env.Gateway(harness.GatewayOpts{})
	v1 := env.Fixture(gw, harness.FixtureOpts{Namespace: "library", DrainOnSIGTERM: true})

	load := harness.StartLoad(t, harness.LoadOpts{Workers: 8},
		func(ctx context.Context, _ int) error { _, err := v1.Echo(ctx, "v1", "x"); return err })
	load.WaitOK(t, 100, 10*time.Second)

	env.Fixture(gw, harness.FixtureOpts{Namespace: "library", Versions: []string{"v1", "v2"}, DrainOnSIGTERM: true})
	v1.Proc.Stop(30 * time.Second)           // SIGTERM: drain, deregister, exit 0
	load.Stop().RequireNoFailures(t)
}
```

Rules:

- No `time.Sleep`. Wait on a condition: `harness.Eventually`,
  `Proc.WaitLog`, `Admin.WaitInstance`, `Admin.WaitDrainState`,
  `Gateway.WaitDeltas`, `Load.WaitOK`.
- Stories may run with `t.Parallel()`: every port comes from
  `harness.FreePort`, every directory from `t.TempDir`.
- Name processes once per Env (`FixtureOpts.Instance`, default
  `<namespace>-<n>`).
- Call helpers that can fail the test from the test goroutine, not from a
  load worker.

## Harness API (short form)

```go
harness.Main(m *testing.M) int                       // from TestMain; owns the binary dir
harness.New(t) *Env
env.Gateway(GatewayOpts{Name, NATS, Environment, NoCallerIDHeader, Args}) *Gateway
env.Cluster(n, GatewayOpts) *Cluster                 // n1..nN, waits for n peers each
  c.Nodes []*Gateway; c.AddNode() *Gateway; c.Live(); c.WaitPeers(n, timeout)
  c.WaitStreamsReplicated(t, n, timeout[, buckets...])  // KV streams at n replicas, leader, current
  c.LB(t, LBOpts) · lb.WaitProbeFails(t, g, timeout)    // probe fails, pulled or not
env.Fixture(gw, FixtureOpts{...}) *Fixture           // waits for ready unless NoWait
  FixtureOpts.FailoverGateways []*Gateway            // --gateway-alt per node
env.Start(name, bin, args...) *Proc                  // any other process
harness.Binary(t, "gateway"|"fixture"|"gwag") string

Proc: Stop(timeout) code · Kill() · Restart() · Signal(sig) · WaitExit(timeout) code
      Running() · ExitCode() · PID() · WaitLog(substr, timeout) · Logs() · CurrentLogs()

Gateway: BaseURL() · CPAddr() · Token() · Restart() · WaitReady() · Health(ctx)
         GraphQL(ctx, q, vars, opts...) (*GQLResponse, error) · MustGraphQL(t, q, vars)
         Post/Get(ctx, path, ..., opts...) · ControlPlane() cpv1.ControlPlaneClient
         Admin() *Admin · Scrape(ctx) / MustScrape(t) *Scrape · MCP(t) *mcpclient.Client
         WaitDeltas(t, before, name, []Want{{Labels, Delta}}, timeout) *Scrape
ReqOpt: Caller("orders/v1") · Header(k, v) · Bearer(tok)    // default X-Caller-Id: e2e

Fixture: Echo(ctx, line, msg, opts...) (Echo, error)  // GraphQL; line "" = latest
         EchoVia(ctx, gw, line, msg) · MustEcho(line, msg)
         EchoREST · EchoConnect (proto) · EchoGRPC(ctx, line, msg, caller) (proto)
         EchoQuery(line) (query, dataPath) · MetricMethod(line)
         SetFaults(Faults{LatencyMs, ErrorRate, Hang, CrashAfter}) · Stats()
         Restart() · WaitReady() · Proc · Addr · ControlAddr

Admin: Services(ctx) · WaitInstance(ns, ver, id, state, timeout)   // state "" = gone
       Graph(ctx, window) · DrainInstance(ctx, id, cancel) · WaitDrainState(id, state, timeout)
       Deprecate/Undeprecate(ctx, ns, ver[, reason]) · Registrations(ctx) · MCPInclude(ctx, glob)
       PutExternalDependent(ctx, x) warnings · DeleteExternalDependent(ctx, id) · ExternalDependents(ctx)
       Do(ctx, method, "/admin/...", body, out)                    // with the bearer

Scrape: Counter(name, Labels) · Samples(name, Labels) · Has(name) · Describe(name, Labels)
harness.Delta(before, after, name, Labels)             // name may omit go_api_gateway_

StartLoad(t, LoadOpts{Workers, Interval, CallTimeout}, fn) *Load
  Counts() · WaitOK(t, n, timeout) · Stop() LoadResult{OK, Failed, Errors} · RequireNoFailures(t)
Eventually(t, timeout, desc, fn) · Consistently(t, d, desc, fn) · Poll · Permanent(err)
FreePort(t) · Ctx(t, d) · EchoMethod(ns, line) · ProtoPackage(ns, line)
```

## Fixture flags

| flag | meaning |
| --- | --- |
| `--kind proto\|openapi\|graphql` | upstream kind (default proto) |
| `--gateway HOST:PORT` | control plane to register with |
| `--gateway-alt HOST:PORT` | another control plane of the cluster, repeatable; the registration moves there when `--gateway` stops answering (controlclient `GatewayAddrs`) |
| `--namespace NS` | namespace |
| `--versions v1,v2` | lines served, one registration (default v1) |
| `--depends SVC:LINE[,LINE]` | dependency, repeatable |
| `--owner TEAM` | manifest owner |
| `--instance ID` | instance id |
| `--addr` / `--control` | service and control listen addresses |
| `--proto LINE=PATH` | register this .proto for LINE (proto kind) |
| `--latency D` | delay every call |
| `--error-rate R` | fail every 1/R-th call (deterministic) |
| `--hang` | never answer; block until the caller gives up |
| `--crash-after N` | exit 3 on call N+1, no deregister |
| `--drain-on-sigterm` / `--drain-timeout D` | controlclient `DrainOnSIGTERM` |
| `--ttl D` | registration TTL |
| `--max-concurrency N` | per-binding `MaxConcurrency` |

Without `--drain-on-sigterm`, SIGTERM deregisters and exits 0. The control
listener serves `GET /stats` and `POST /faults` (replaces the fault set).
Proto packages are `fixture.<ns>.<line>`; OpenAPI paths are
`/<ns>/<line>/echo`; GraphQL endpoints are `/<line>/graphql`.

## Gateway behaviour the stories rely on

- Connect (`/api/connect`) takes the caller id from gRPC metadata, so a
  Connect client sends `Caller-Id`, not `X-Caller-Id`.
- Dispatch metric `code` label: `ok`, or the gRPC code name
  (`Internal`, `DeadlineExceeded`) for every kind. See
  `TestDispatchCodeLabelCanonical`.
