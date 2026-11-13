# Registration clients for TypeScript and Python

A service registers with the gateway's control plane
(`gateway.controlplane.v1.ControlPlane`, gRPC). Go services use
[`controlclient`](./services.md#control-plane-with-controlclient). TypeScript
and Python services use the two clients below. Both do the same four
things: announce, wait for the gateway's ok, heartbeat, deregister.
Status: experimental.

| | Package | Transport | Dependencies |
|---|---|---|---|
| TypeScript | [`ts/registration`](../ts/registration) (`@gwag/registration`, private) | `@connectrpc/connect-node` gRPC client | `@bufbuild/protobuf`, `@connectrpc/connect`, `@connectrpc/connect-node` |
| Python | [`py/gwag-registration`](../py/gwag-registration) (uv project, `gwag_registration`, private) | `grpclib` | `betterproto2[grpclib]` (`grpclib`, `h2`, `multidict`; no compiled extension) |

Each is its own package, so `@gwag/events` and `gwag-events` take no
gRPC dependency. Python uses `grpclib` rather than `grpcio` because it
is pure Python and the stubs come from the same betterproto2 compiler
the event code uses.

## The control plane schema is on the gateway

With the [Schema Registry](./schema-registry.md) on (`gwag up
--schema-registry`), the gateway serves `control.proto` as the subject
`gateway.controlplane.v1.ControlPlane`:

```
curl http://HOST:8080/schema-registry/subjects/gateway.controlplane.v1.ControlPlane/versions/latest
```

The reply has `schema` (the `.proto` text, comments included),
`schemaType` `PROTOBUF` and an `id` of gwag's default context (an
imported registry has its own context and id space). Save `schema` as a `.proto` file and run
your own generator on it: protoc-gen-es, betterproto2, protoc-gen-go,
buf. `gwag gen --lang ts|python` does it with the generators your
project pins (see [`events.md`](./events.md)).

The two packages here do that step with the generators they pin and
commit the output. `examples/events/registration.sh` fetches the
subject from a running gateway, regenerates both clients and checks
they equal the committed ones.

## TypeScript

```ts
import { readFileSync } from "node:fs";
import { register } from "@gwag/registration";

const reg = await register({
  gateway: "gateway:50090",                 // the control-plane address
  serviceAddr: "billing:50051",             // where this service's gRPC server listens
  applicationKey: process.env.GWAG_APP_KEY, // optional
  ttlSeconds: 30,
  deregisterOnSignal: ["SIGTERM", "SIGINT"],
  services: [{
    namespace: "billing",
    version: "v1",
    protoSource: readFileSync("billing.proto", "utf8"),
    subscribes: [{ channel: "orders.v1.OrderEvents", group: "billing" }],
  }],
});
// register() returned: the gateway said ok. Start serving.
// ...
await reg.close(); // deregisters
```

## Python

```python
reg = await register(Options(
    gateway="gateway:50090",
    service_addr="billing:50051",
    application_key=os.environ.get("GWAG_APP_KEY"),
    ttl_seconds=30,
    deregister_on_signal=(signal.SIGTERM, signal.SIGINT),
    services=[Service(
        namespace="billing",
        version="v1",
        proto_source=Path("billing.proto").read_text(),
        subscribes=[Subscription("orders.v1.OrderEvents", "billing")],
    )],
))
# register() returned: the gateway said ok. Start serving.
await reg.close()  # deregisters; `async with await register(...)` does the same
```

## Profile and verification

A service profile (`gwag.json`, written by `gwag ctl services init -o
gwag.json`) says what the service provides, depends on and listens on.
`loadProfile` / `load_profile` reads it and returns one service per
provided version, sorted `v1`, `v2`, ..., `unstable`. Spec files are read
relative to the profile's directory; proto imports are the other `.proto`
files under the import directories. The format is in
[`gw/profile/profile.go`](../gw/profile/profile.go). The clients read JSON;
Python also reads YAML when PyYAML is installed.

`healthUrl` / `health_url` and `specUrl` / `spec_url` ask the gateway to
verify each instance before it routes to it: the address is reachable, the
health URL answers 200, the spec served at the spec URL equals the
registered one. It checks again periodically. Each is a path on the
service's host or an absolute URL on that host. A binding that is not
verified is WAITING, and the client logs why (`registration: waiting:
billing/v1 red: health /healthz: status 500`). The profile sets both
(`health`, and `url` per version). The whole pattern, with the CI check
and the deploy gate: [`integrate.md`](./integrate.md).

```ts
import { loadProfile, register } from "@gwag/registration";

const services = loadProfile("gwag.json", "billing:50051");
const reg = await register({ gateway: "gateway:50090", serviceAddr: "billing:50051", services });
// or by hand: { namespace: "billing", openapiSpec, healthUrl: "/healthz", specUrl: "/openapi.json" }
```

```python
from gwag_registration import Options, load_profile, register

services = load_profile("gwag.json", "billing:50051")
reg = await register(Options(gateway="gateway:50090", service_addr="billing:50051", services=services))
# or by hand: Service(namespace="billing", openapi_spec=spec, health_url="/healthz", spec_url="/openapi.json")
```

## Behaviour

The same as `controlclient`:

- **Waits for ok.** `register` returns when the gateway reports every
  binding ready. A binding whose `depends` or `subscribes` have no ready
  provider is WAITING (the [start gate](./services.md)); the client
  polls `Heartbeat` and logs what it waits for. A service that listens
  on a channel nobody has declared yet does not start.
- **Gateway down.** An unreachable gateway (`UNAVAILABLE`,
  `DEADLINE_EXCEEDED`, `RESOURCE_EXHAUSTED`, `ABORTED`, a refused
  connection) is retried with backoff, 250 ms up to 5 s, forever. A
  refused register (bad request, dependency cycle, rejected key) raises.
  Abort the wait with `signal` (TypeScript) or by cancelling the task
  (Python).
- **Heartbeat** at TTL/3 (10 s when the gateway reports no TTL). A
  heartbeat answered `ok: false` (the gateway restarted or evicted the
  registration) registers again on the same gateway; `reg.id` changes.
- **Several gateways.** `gateway` takes a list. When a heartbeat cannot
  reach the gateway that holds the registration, the client registers on
  the next address.
- **Deregister** on `close()`, and on the signals in
  `deregisterOnSignal` / `deregister_on_signal`. After a signal the
  client re-raises it so the process exits as it would have. Off by
  default: catching a signal changes the process's signal handling.
- **Application key.** Sent on every call as gRPC metadata
  `authorization: Bearer <key>`. The client sends it when set; a
  gateway that checks application keys reads it there.

`reg.bindings` reports the start-gate state of each binding, and
`reg.presentLines(service)` / `reg.present_lines(service)` the lines of
a dependency that are ready.

## Not in the clients

Drain (`DrainInstance`) and the other control-plane calls (peers,
`SignSubscriptionToken`, stable and deprecation controls) are in the
generated stubs, not in the wrappers. `BuildTag` (refuse `unstable` in
release builds) is Go only. OpenAPI and GraphQL bindings are supported
by field (`openapiSpec`, `graphqlEndpoint`) but only proto bindings are
tested end to end.

## Tests

```
pnpm --dir ts/registration test        # in-process control plane
(cd py/gwag-registration && uv run pytest)
examples/events/registration.sh        # a real gateway; needs go, node >= 24, pnpm, uv
```

`examples/events/check.sh` runs all three and the drift checks on the
generated stubs (`ts/registration/gen.sh`, `py/gwag-registration/gen.sh`).
