# e2e

End-to-end stories: real gateway and service processes, driven over
their real ports. Every file has the `e2e` build tag, so `go build ./...`
and `go test ./gw/...` never compile or run any of it.

```
go test -tags e2e ./e2e/...                      # all stories
go test -tags e2e ./e2e/ -run TestS0 -v          # one story
E2E_LOG_LINES=200 go test -tags e2e ./e2e/...    # longer log tails on failure
E2E_KNOWN_BUGS=1 go test -tags e2e ./e2e/ -run KnownBug   # reproduce open bugs
E2E_KAFKA=1 go test -tags e2e ./e2e/ -run TestS12 -v      # the Kafka story (docker)
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
- `kafka-fixture/` — the Go services of the Kafka story (orders
  producer, billing listener, retry/DLQ probe). Its own Go module, so
  franz-go stays out of the root `go.mod`; the harness builds it from
  that directory.
- `*_test.go` here — the stories. `known_bugs_test.go` holds tests for
  open gateway bugs; they skip unless `E2E_KNOWN_BUGS=1`.

## Stories

| test | story |
| --- | --- |
| `TestS0HappyPath` | one gateway, a proto, an OpenAPI and a GraphQL service, every ingress, admin and metrics |
| `TestS1RollingRelease` | provider and consumer move v1 → v2 under load |
| `TestS2ColdFleetStart` | five services with a dependency DAG start in random order; a cycle is refused |
| `TestS3InstanceCrash` | SIGKILL replicas one at a time under load |
| `TestS4ClusterOps` | 3-node cluster behind an LB: node loss, registrations move |
| `TestS4bSigtermInRotation` | SIGTERM a node still in the LB's rotation |
| `TestS5Isolation` | a slow, then hung service backs up its own pool only |
| `TestS6SchemaGovernance` | locked vN, mutable unstable, deprecation, schema export |
| `TestS8SubscriptionFailover` | a `ps_sub` client moves to another node when its node dies |
| `TestS9ExternalDependents` | an external customer declared through the admin API |
| `TestS9bDeclarationSinks` | external-dependent changes to a webhook and a file per node |
| `TestS10AdminRoles` | three admin readers, public and private services |
| `TestS11TypedEvents` | typed event channels on a 3-node cluster: streams, start gate and graph, Go group delivery, TypeScript and Python listeners, `ps_sub` / `ps.pub` bridge, dead letters, NATS node loss |
| `TestS12KafkaEvents` | the same channels over Kafka with the Schema Registry serde, services registered on a 3-node cluster: start gate, graph and sunset, Go group, TypeScript and Python delivery, Confluent's Java deserializer reads the records, `gwag gen --registry` import of a Confluent-written topic, retry topic and DLQ, gateway node loss |
| `TestS13ReplicaSwap` | replica swap upgrade under load: 3 nodes on the v2.11.0 gateway, 3 at HEAD join one at a time, each old node is evacuated (after each, every stream at R3 and on no evacuated node), stopped and forgotten, then the cluster is finalized; no failed call, no lost event, state intact, every stream on the new nodes |
| `TestS13bRollbackBeforeFinalize` | 2 HEAD nodes join a v2.11.0 cluster and leave again before finalize (the last with `evacuate --leave`); the old cluster keeps its level, streams and meta quorum |
| `TestS14FailureModes` | failure modes under traffic through a health-checked LB, 3 nodes: the only instance of a service killed, one of two instances killed, 1 of 3 nodes killed, 2 of 3 nodes killed (no JetStream quorum: the survivor keeps routing, writes fail); per traffic kind (GraphQL, REST, gRPC, `ps.pub`, `ps_sub`, typed events) the failed calls and recovery time, logged as `S14 \|` lines for [`docs/running.md`](../docs/running.md) |
| `TestS14bStandaloneRestart` | one node with JetStream on disk killed and restarted: KV state kept, the service registers again by heartbeat |
| `TestS15GatedDeploy` | two replicas register with a health URL and a spec URL (`--verify`); both green and `gwag service wait --green` exits 0; one replica's health fails: red with the reason, no traffic to it while the other serves, `service wait` for it exits 3, `service_verification{state="red"}` is 1; it recovers to green |

`TestS11TypedEvents` runs the TypeScript listener when `node` (>= 24)
and `pnpm` are on `PATH`, and the Python listener when `uv` is; it
runs `pnpm install --frozen-lockfile` and `uv sync --locked` first.
Without a tool, that step is skipped and says why.

`TestS12KafkaEvents` skips unless `E2E_KAFKA=1` is set and docker
answers. It starts `apache/kafka` and `confluentinc/cp-schema-registry`
(the tags `examples/events/kafka-stack.sh` pins; `KAFKA_IMAGE` /
`SR_IMAGE` override) on free ports and removes them at the end. The
TypeScript and Python listeners (`examples/events/ts-kafka/src/listen.ts`,
`examples/events/py-kafka/listen.py`) and the import step (Confluent's
Python serializer in `examples/events/go-kafka/confluent`) need the
same tools as in S11 and skip the same way. The gateway carries no
event traffic in this story; TypeScript and Python have no
control-plane client, so the test registers their `Subscribes`.

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
env.ClusterWithNodeArgs(n, opts, func(name, dataDir) []string) *Cluster   // plus per-node flags
  c.Nodes []*Gateway; c.AddNode() *Gateway; c.Live(); c.WaitPeers(n, timeout)
  c.WaitStreamsReplicated(t, n, timeout[, buckets...])  // KV streams at n replicas, leader, current
  c.WaitStreamReplicas(t, n, timeout, streams...)       // the same for any named stream
  c.NATS(t, opts...) *nats.Conn · c.NATSURLs() · c.JetStream(t)   // clients on the nodes' NATS ports
  c.LB(t, LBOpts) · lb.WaitProbeFails(t, g, timeout)    // probe fails, pulled or not
  lb.Next()                                             // next node in rotation, no call counted (a WebSocket)
env.Fixture(gw, FixtureOpts{...}) *Fixture           // waits for ready unless NoWait
  FixtureOpts.FailoverGateways []*Gateway            // --gateway-alt per node
env.Start(name, bin, args...) *Proc                  // any other process
env.StartWith(name, ProcOpts{Dir, Env, Stdin}, bin, args...) *Proc   // with cwd, env, open stdin
harness.Binary(t, "gateway"|"fixture"|"gwag"|"kafka-fixture") string
harness.ReleaseBinary(t, "v2.11.0", "gateway") string  // built from a tag (archive of the tag), cached in the user cache dir (E2E_CACHE_DIR)
GatewayOpts{Binary, Legacy}                          // run another binary; Legacy drops flags v2.11.0 lacks
env.NamedCluster(names, opts) *Cluster · c.NewNode(opts) *Gateway · c.StartNode(g)   // allocate first, start later, one at a time
lb.Add(g) · g.PeersView(ctx) · g.NodeID(t) · g.Evacuate(t, id, leave, timeout) · g.Forget(t, id, timeout) · g.Finalize(ctx, level, dryRun)
harness.DockerAvailable() string                       // why not, or ""
harness.StartKafkaStack(t) *KafkaStack                 // Kafka + Schema Registry in docker, free ports
  s.Brokers · s.SRURL · s.Name (Name:9094 inside the network) · s.Env() · s.Exec(ctx, args...)

Proc: Stop(timeout) code · Kill() · Restart() · Signal(sig) · WaitExit(timeout) code
      Running() · ExitCode() · PID() · WaitLog(substr, timeout) · Logs() · CurrentLogs()
      CloseStdin()                                        // with ProcOpts.Stdin

Gateway: BaseURL() · CPAddr() · Token() · Restart() · WaitReady() · Health(ctx)
         GraphQL(ctx, q, vars, opts...) (*GQLResponse, error) · MustGraphQL(t, q, vars)
         Post/Get(ctx, path, ..., opts...) · ControlPlane() cpv1.ControlPlaneClient
         Admin() *Admin · Scrape(ctx) / MustScrape(t) *Scrape · MCP(t) *mcpclient.Client
         WaitDeltas(t, before, name, []Want{{Labels, Delta}}, timeout) *Scrape
         NATSURL() · Sign(ctx, channel, ttl) · Subscribe(t, query) *Subscription
         Publish(ctx, channel, payload, tok) · PublishTyped(ctx, channel, payload, payloadType, tok)
ReqOpt: Caller("orders/v1") · Header(k, v) · Bearer(tok)    // default X-Caller-Id: e2e

Fixture: Echo(ctx, line, msg, opts...) (Echo, error)  // GraphQL; line "" = latest
         EchoVia(ctx, gw, line, msg) · MustEcho(line, msg)
         EchoREST · EchoConnect (proto) · EchoGRPC(ctx, line, msg, caller) (proto)
         EchoRESTVia(ctx, gw, line, msg) · EchoGRPCVia(ctx, gw, line, msg, caller)
         EchoQuery(line) (query, dataPath) · MetricMethod(line)
         SetFaults(Faults{LatencyMs, ErrorRate, Hang, CrashAfter}) · Stats()
         Restart() · WaitReady() · Proc · Addr · ControlAddr

Admin: Services(ctx) · WaitInstance(ns, ver, id, state, timeout)   // state "" = gone
       Graph(ctx, window) · DrainInstance(ctx, id, cancel) · WaitDrainState(id, state, timeout)
       Deprecate/Undeprecate(ctx, ns, ver[, reason]) · Registrations(ctx) · MCPInclude(ctx, glob)
       PutExternalDependent(ctx, x) warnings · DeleteExternalDependent(ctx, id) · ExternalDependents(ctx)
       ReviewExternalDependent(ctx, id, reviewAt) · Notifications(ctx) · DismissNotification(ctx, id)
       Do(ctx, method, "/admin/...", body, out)                    // with the bearer

NewHookSink(t) *HookSink        // real HTTP receiver for --declarations-hook
  .URL · Requests() · WaitN(t, n, timeout, settle)   // exactly n, none more for settle
VerifyHookSignature(req, secret) error

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
| `--public` | manifest `public=true` (admin readers without a token see it) |
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
