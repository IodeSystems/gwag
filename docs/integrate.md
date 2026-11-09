# Prep, lint, gated deploy

The common way a service joins gwag:

1. **Cluster up.** A gateway (or cluster) runs in the environment.
2. **Profile.** `gwag service init` writes `gwag.yaml`: what the
   service provides (versions, spec files, the URLs where the running
   service serves each spec and its health check), what it depends on,
   which event channels it listens on.
3. **Lint in CI.** `gwag service check` validates the profile, checks
   a running build against it, and checks that its dependencies exist
   on the target environment.
4. **Deploy.** The service registers (registration client) or the
   gateway pulls it (external or legacy service). gwag verifies each
   instance and routes to it only when it is green.
5. **Gate promotion.** `gwag service wait --green` exits 0 when the
   new instance is green, non-zero with the reasons when it is red.

Two kinds of service take this path:

- **A service you own** runs a registration client (Go
  `controlclient`, `@gwag/registration`, `gwag_registration`). It
  loads `gwag.yaml` at start and pushes it with its registration.
- **An external or legacy service** you cannot change much serves the
  profile as JSON at `/.well-known/gwag.json`. An operator adds it by
  URL; the gateway registers it and holds the registration.

Both get the same checks and the same green / red state.

## Green and red

An instance is **green** when all of these pass:

1. The registered address accepts a connection, and the health URL
   answers 200.
2. The spec the instance serves at its spec URL equals the spec it
   registered. The comparison is on a canonical form: key order,
   formatting and deprecation marks do not count (gwag and gat mark
   older versions deprecated themselves). GraphQL services are
   introspected again instead.
3. Its declared `depends` have a ready instance on an accepted line,
   and each subscribed channel is declared by a registered service.
   This is the existing [start gate](./services.md#start-gate-depends).

There is no smoke call: gwag does not send a request to an operation.

A **red** instance gets no traffic. Green instances of the same line
keep serving. The reason is in the admin API (`/admin/services`,
`/admin/graph`), the admin UI (dependency graph, instance table,
"Verified" column), `ListServices`, and the metrics
`service_verification{namespace,version,state}` and
`service_verification_changes_total`
([operations.md](./operations.md)). The gateway
probes a green instance every 30 s and a red one every 2 s
(`WithVerification`); a red instance turns green on its first passing
probe.

A new instance that is not green yet is reported `WAITING`, the same
state as an instance whose dependency is missing. A registration
client from before verification therefore waits for it the way it
waits for a dependency. An instance that was green and turns red is
also reported `WAITING`, with `verification: RED` and the reasons.

An instance that declares no health or spec URL is not verified:
`verification` is empty (`none` in the admin API) and it is routed as
soon as the start gate passes, as before.

## 1. Profile: `gwag service init`

From the spec files you already have:

```bash
gwag service init \
  --spec v1=api/billing-v1.json --spec v2=api/billing-v2.json \
  --url '/api/schema/openapi?version={version}' \
  --health /healthz \
  --depends library:v1,v2 \
  --subscribe orders.v1.OrderEvents@billing \
  --owner-team payments --owner-contact '#payments'
```

```yaml
# gwag service profile. Schema: `gwag service schema`. Check: `gwag service check`.
service: billing
owner:
  team: payments
  contact: '#payments'
health: /healthz
provides:
  v1:
    kind: openapi
    file: api/billing-v1.json
    url: /api/schema/openapi?version=v1
  v2:
    kind: openapi
    file: api/billing-v2.json
    url: /api/schema/openapi?version=v2
depends:
  library:
    - v1
    - v2
subscribes:
  - channel: orders.v1.OrderEvents
    group: billing
```

- `--spec FILE` takes a `.proto` entry file or an OpenAPI document; the
  kind and the namespace (proto file stem, OpenAPI `info.title`) come
  from the file. `--service` overrides the namespace. `-I` sets proto
  import directories.
- `--url` is where the running service serves each version's spec.
  For a gat service that is gat's schema route:
  `/api/schema/openapi?version={version}` (OpenAPI) or
  `/api/schema/proto?version={version}` (FileDescriptorSet). A plain
  gRPC service can serve its `.proto` file on any HTTP path.
- `--health` is a path, or an absolute URL on the service's host (a
  gRPC service usually serves health on another port:
  `http://orders:8081/healthz`).
- `--graphql /graphql` provides a GraphQL endpoint instead of a spec
  file.
- `-o gwag.json` writes JSON: the file to serve at
  `/.well-known/gwag.json`, and the one the TypeScript and Python
  clients load.
- `snapshot: frozen/billing-v1.json` under a version (add it by hand)
  makes `service check` compare the spec file with a committed
  snapshot, as `gwag schema snapshot check` does
  ([lifecycle.md](./lifecycle.md#frozen-spec-snapshots)).

`gwag service schema` prints the JSON Schema of the file, for editors
(`$schema:` key) and CI linters. Unknown keys are an error.

## 2. Register from the profile

Go:

```go
svcs, err := controlclient.LoadProfile("gwag.yaml", "http://billing:8080")
if err != nil {
	log.Fatal(err)
}
reg, err := controlclient.SelfRegister(ctx, controlclient.Options{
	GatewayAddr: "gateway:50090",
	ServiceAddr: "http://billing:8080",
	Services:    svcs,
})
```

`LoadProfile` reads each spec file and sets `Depends`, `Owner`,
`Public`, `Subscribes`, `HealthURL` and `SpecURL`. Start the HTTP (or
gRPC) server before `SelfRegister`: the gateway probes the instance
while it registers, and a service that is up is `READY` in the
Register response. `SelfRegister` blocks while the instance is red and
logs why:

```
controlclient: waiting: billing/v1 red: health http://billing:8080/healthz: status 503, want 200
controlclient: ready, starting
```

TypeScript and Python: [`registration-clients.md`](./registration-clients.md#profile-and-verification).

Without the profile, set the two fields by hand:
`controlclient.Service{..., HealthURL: "/healthz", SpecURL: "/openapi.json"}`.

## 3. Lint in CI: `gwag service check`

```bash
gwag service check                                   # the profile and its spec files
gwag service check --url http://localhost:8080       # + a running build
gwag service check --url http://localhost:8080 --gateway staging-gw:50090
gwag service check ... --json                        # machine-readable
```

Checks, and the exit code when one fails:

| Exit | Class | Checks |
|---|---|---|
| 3 | profile | the file parses, no unknown keys; service name, versions (`vN`, `unstable`), kinds, depends lines, channels; each spec file reads and parses; no breaking change against `snapshot` |
| 4 | reachability | `--url` accepts a connection; each health URL answers 200; for proto versions a gRPC connection to `--grpc-addr` (default: the `--url` host and port) becomes ready |
| 5 | spec | each version's spec at its `url` equals its `file` (same comparison as the gateway); the profile served at `/.well-known/gwag.json` equals the local one (ignoring `file`, `imports`, `snapshot`); GraphQL endpoints answer introspection |
| 6 | depends | with `--gateway`: each dependency has a line in its accepted set with a live replica on that gateway |

0 is ok, 1 an error running the check, 2 a usage error. With several
failures the lowest code wins. A served profile is checked when one
answers; `--well-known` makes it required (for a pulled service).
A missing health URL or spec `url` is a warning, not a failure: the
gateway then checks less.

A CI job for a service you own:

```bash
gwag service check                              # on every push
./billing --listen :8080 &                      # the build under test
gwag service check --url http://localhost:8080 --gateway "$STAGING_GW"
```

## 4. Deploy

### A service with a registration client

Deploy as usual. The instance registers; the gateway probes it at once
and on every interval. Nothing else to do.

### An external or legacy service

The service serves `gwag.json` at `/.well-known/gwag.json`, with `url`
on every proto and openapi version (the gateway reads the spec from
there) and, for a proto service, `address` (its gRPC host:port; the
default is the profile URL's origin):

```json
{
  "service": "ledger",
  "address": "ledger.internal:9090",
  "health": "http://ledger.internal:8080/healthz",
  "provides": {
    "v1": {"kind": "proto", "url": "http://ledger.internal:8080/ledger.proto"}
  }
}
```

If you cannot change the service, serve the file from anything on the
same host. Then:

```bash
gwag services add --profile-url http://ledger.internal:8080/.well-known/gwag.json
gwag services profiles                          # list pulled services
gwag services remove --profile-url http://ledger.internal:8080/.well-known/gwag.json
```

or `POST /admin/services/profiles {"url": ...}` (admin token).

The gateway reads the profile and each spec, refuses a bad one at
once, stores the URL (in the cluster KV, so it survives restarts),
registers the service itself and heartbeats it: the service sends
nothing. In a cluster one node holds the registration (the live node
with the lowest id); when that node goes, the next one registers the
profile again. The profile and specs are read when the profile is
added and when a node registers it again. If the service later serves
a different spec, the instance turns red; add the profile again to
register the new spec (a `vN` with a different spec is refused, as for
any registration: cut `vN+1`).

## 5. Gate promotion: `gwag service wait`

```bash
gwag service wait --gateway "$GW" --service billing:v2 --green --timeout 2m
gwag service wait --gateway "$GW" --service billing --instance "billing-$GIT_SHA" --green
```

Green: the instance is `READY` (start gate passed) and its
verification is green, or not declared. Exit codes: 0 green; 3 not
green at the timeout, with each instance's reasons (failed checks,
missing dependencies); 4 no matching instance at the timeout; 1 an
error; 2 usage. Without `--instance`, every instance of the line must
be green; `--any` accepts one. `--json` prints the final state.

A deploy step:

```bash
kubectl rollout status deploy/billing
if ! gwag service wait --service billing:v2 --instance "billing-$GIT_SHA" --green --timeout 2m; then
  kubectl rollout undo deploy/billing
  exit 1
fi
```

## Probe safety

- Probes go only to the registered address. A health or spec URL is a
  path on it, or an absolute URL on the same host (any port); Register
  refuses another host, so a registration cannot make the gateway call
  a metadata service or another tenant. Redirects are not followed.
- Each probe has a timeout (5 s default).
- Adding a pulled profile is an admin operation (admin token). The
  profile URL may name any host unless
  `WithVerification(VerifyOptions{ProfileHosts: []string{"*.internal"}})`
  restricts it. Every URL in a pulled profile (address, specs, health,
  endpoints) must be on the profile URL's host.
- A pushed registration's probes need no extra rights: the gateway
  already dials that address to dispatch.

## Limits

- A gateway older than verification ignores `verify`: it routes to an
  instance as soon as the start gate passes. In a mixed-version
  cluster an older node keeps routing to an instance that turned red
  after it was green.
- A failed probe turns a green instance red at once; there is no
  failure threshold. A health endpoint that flaps takes the instance
  in and out of rotation.
- `service check --gateway` checks that dependency lines are served;
  it does not check subscribed channels.
- A pulled proto service must serve its `.proto` source (one file, no
  imports) at `url`; the gateway compiles it as a registration
  client's `ProtoSource`. Imports work only for pushed services.
- The profile has no per-version address: one address serves every
  version it provides (several instances with different addresses
  are several registrations, as before).

## Related

- [`services.md`](./services.md) — controlclient, start gate, drain, app keys
- [`registration-clients.md`](./registration-clients.md) — TypeScript and Python clients
- [`lifecycle.md`](./lifecycle.md) — versions, snapshots, retirement
- [`gat.md`](./gat.md#schema-views-per-version) — gat's schema routes per version
