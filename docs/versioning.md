# Breaking changes with clients that are hard to move

You have to change an API in a way old clients cannot take: a field
changes type, an operation goes away. Some of those clients will not
move for months: a mobile app release in review, a partner who ships
twice a year, a batch job nobody owns. You serve the old version and
the new one side by side until the last caller has moved, then delete
the old one.

This page is the pattern, with the gwag pieces that make each step
visible. The runnable version is
[`examples/versioning/`](../examples/versioning): an `orders` service
on [gat](./gat.md) with v1 and v2 in one process, Go and TypeScript
clients for each, a TypeScript service that depends on v1, and
`walkthrough.sh`, which takes v1 from deprecated to deleted against a
real `gwag up`.

The example's change, used throughout:

| | v1 (frozen) | v2 |
|---|---|---|
| Money | `amount`, float dollars | `amountCents` int64 + `currency` |
| Customer | `customerName` | `customer` (rename) |
| Status | `open`, `closed` | `open`, `closed`, `refunded` |
| Refunds | not shown | `refundedCents` |
| Operations | `getOrder`, `listOrders`, `applyDiscount` | `getOrder`, `listOrders`, `refundOrder` |

## Three ways to keep v1 alive

| | How | Costs |
|---|---|---|
| **Keep the old deployment** | The v1 binary keeps running from a frozen branch; v2 is a new service. | Every fix that touches both lands twice. Both write the same data, so the store's schema must suit both. The old branch's dependencies age; security patches go to code nobody works on. |
| **One binary, v1 as adapters** | v2 has the real handlers. A v1 package maps each v1 request onto the domain and the domain result back to v1's wire types. Both versions are registered from one process. | Adapter code and tests for every v1 operation. Each lossy mapping is a decision you write down. |
| **Gateway transforms** | The gateway rewrites v1 requests into v2 calls and v2 responses into v1 shapes. | The mapping lives outside the service's repository and tests, in a configuration language that is weaker than code. gwag's [middleware](./middleware.md) injects headers and arguments; it does not rewrite shapes, by design. |

Use adapters. The mapping is code next to the code it maps, it runs
in the service's own tests, there is one deployment and one store,
and deleting v1 later is deleting a package. Keeping the old
deployment is fine when v1 retires within weeks and nothing in it
changes.

## Mapping rules

What the v1 adapter does for each kind of change. Each row is a
decision: write it in the v1 package and pin it with a test
([`map.go`](../examples/versioning/internal/api/v1/map.go),
[`map_test.go`](../examples/versioning/internal/api/v1/map_test.go)).

| Change | v1 request into the domain | Domain result out to v1 | Example |
|---|---|---|---|
| Field renamed | Read the old name. | Write the old name. | `customer` shows as `customerName` |
| Type changed | Convert with a stated rounding rule. | Convert; say where it is exact. | `minAmount` dollars round to the nearest cent, half away from zero; cents/100 is exact for every cent value |
| Field added | Leave the domain default. | Leave it out. | `refundedCents` |
| New data the old shape cannot hold | Imply the default. | Refuse that record with an error that names the new version, or leave it out of lists. Never show it as something it is not. | v1 orders are USD; `getOrder` of a EUR order is 409 `order o-3 is in EUR; v1 shows only USD orders, use v2`; `listOrders` skips it |
| Enum value added | n/a | Map to the closest old value. An old generated client fails on a value it does not know. | `refunded` shows as `closed` |
| Field removed | Ignore it. | Compute it, or send a documented constant. | |
| Operation removed | Keep it as v1-only code over the domain, with a sunset date. | After the date: 410 with the reason. | `applyDiscount`, 410 `applyDiscount was removed on 2027-06-30; orders v2 has no discounts, use refundOrder` |
| Operation added | n/a | v2 only. | `refundOrder` |
| Errors | Map domain errors in the v1 package to the status and text v1 clients parse. | | `order o-404 not found`, 404 |

Put the sunset date in configuration, not in a constant: the date
moves when a client needs more time, and moving it should not need a
release (`-discount-sunset` in the example).

## Domain in the middle

With two versions it is tempting to write v1 as an adapter onto v2's
handlers. When v3 ships, v1 then runs through v2's adapter into v3, the
lossy rules of both stack, and retiring v2 means rewriting v1. Map
every version onto the domain instead:

```
internal/domain/    Order, Store: no version
internal/api/v2/    v2 wire types <-> domain, calls the store
internal/api/v1/    v1 wire types <-> domain, calls the store
```

`internal/api/v1` imports `internal/domain`, never `internal/api/v2`.
When v3 ships, v2 becomes a package like v1, and v1 does not change.
The domain is where the behavior is; each version is a translation.

## Freeze each version's spec

The specs are generated from code (huma, gat), so a refactor can
change v1's contract without anyone editing it. Commit each version's
spec as a snapshot and compare in CI. The example keeps:

```
api/orders/v1/openapi.json   what gwag registers for v1
api/orders/v1/proto.json     gat's gRPC descriptor for v1 (Go clients are generated from it)
api/orders/v2/openapi.json
api/orders/v2/proto.json
api/orders/schema.graphql    one GraphQL schema for all versions
```

[`snapshot.sh`](../examples/versioning/snapshot.sh) builds the
service in process (no server), writes the generated specs, and runs
`gwag ctl schema snapshot check` on each
([lifecycle.md](./lifecycle.md#frozen-spec-snapshots)). A rename on
v1 fails it (excerpt):

```
[BREAKING] removed: /components/schemas/Order/properties/customerName
[INFO] added: /components/schemas/Order/properties/customer
FAIL api/orders/v1/openapi.json: breaking change to a frozen version
[BREAKING] removed: /message_type[Order]/field[customerName]
FAIL api/orders/v1/proto.json: breaking change to a frozen version
[BREAKING] field removed: orders_v1_Order.customerName
FAIL api/orders/schema.graphql: breaking change to a frozen version
```

`./snapshot.sh update` rewrites the snapshots and refuses a breaking
change; `--allow-breaking` is for the day v1 is removed.

What a frozen version may still take:

- **Allowed:** a new optional field in a response, a new optional
  argument or input field, a new operation, a fix that keeps the
  shape.
- **Not allowed:** removing or renaming a field, changing its type,
  a new required argument, a new enum value in a response (old
  generated clients reject it; the OpenAPI classifier also reports an
  `enum` change as breaking).
- **Proto field numbers.** gat and gwag number the fields of a proto
  synthesized from OpenAPI 1..N in property order. A new property
  that sorts between two others renumbers the fields after it, which
  breaks every compiled gRPC client. The proto snapshot reports it as
  breaking; add the field to the new version instead. In a
  hand-written `.proto`, never reuse a number, and mark removed
  fields `reserved` by number and name.

GraphQL is one schema across versions, so its snapshot changes when
v2 changes. Treat it like any client-facing schema:
`gwag ctl schema diff --strict` against the live gateway.

## Contract tests

Snapshots fix the shape. Behavior needs requests and answers. Before
rewriting v1 as an adapter, record what the old implementation
answered, then replay it against the adapter
([contract-tests.md](./contract-tests.md)).

The example keeps the old implementation as
[`cmd/legacy`](../examples/versioning/cmd/legacy/main.go): v1 only,
float amounts, its own store. It is not deployed. `check.sh` records
it again on every run and compares with the committed goldens, and
compares its OpenAPI spec with the v1 snapshot, so both are known to
describe what clients saw:

```
ok   testdata/contract/v1 = legacy answers
ok   legacy v1 spec = api/orders/v1/openapi.json
```

The adapter runs the goldens in a normal test:

```go
func TestV1Contract(t *testing.T) {
	contract.Run(t, newService(t, Options{}).Handler, "../../testdata/contract/v1")
}
```

Goldens cover the cases you thought of. A mapping rule needs a test
over its whole range. The example's discount was float arithmetic in
the legacy code and integer cents in the adapter. Over every amount up
to $1000 and every percent, 7432 of 10,000,100 pairs differ: at exact
half cents, float error made the legacy code round down ($0.29 at 50%
gave $0.14; the adapter gives $0.15). None of the goldens hit it. The
example keeps the integer result on purpose and pins the count in
`TestDiscountAgainstLegacy`; the other choice is to copy the float
code. Make the choice; do not find out from a customer.

Run contract tests against the service, not through the gateway:
gwag's REST ingress keeps an upstream error's status but wraps its
body, so a v1 client calling through `/api/ingress` gets
`{"error": "openapi: GET .../v1/orders/o-3: 409 Conflict: {...}"}`
where the service answers `{"title": "Conflict", "status": 409,
"detail": "..."}`.

## Registering both versions

gat serves each version from its own huma API and builds one GraphQL
schema, one gRPC package per version, and one OpenAPI document per
version ([gat.md](./gat.md#several-versions-in-one-process)):

```go
apiV2 := humaAPI(mux, "v2", "2.0.0") // OpenAPIPath /v2/openapi, ...
v2.Register(apiV2, g, &v2.API{Store: store})
apiV1 := humaAPI(mux, "v1", "1.0.0")
v1.Register(apiV1, g, &v1.API{Store: store, DiscountSunset: sunset})

gat.RegisterHumaVersion(apiV1, g, "v1")
gat.RegisterHumaVersion(apiV2, g, "v2")
gat.RegisterHuma(apiV2, g, "/api")
gat.RegisterGRPC(mux, g, "/api/grpc")
```

Register each version with gwag as its own registration, so one line
can be drained while the other serves
([`cmd/orders`](../examples/versioning/cmd/orders/main.go)):

```go
for _, v := range []string{"v1", "v2"} {
	spec, _ := svc.Gat.OpenAPISpec(v)
	controlclient.SelfRegister(ctx, controlclient.Options{
		GatewayAddrs: gateways,
		ServiceAddr:  "http://orders:18610",
		InstanceID:   "orders-" + v + "@orders:18610",
		Services:     []controlclient.Service{{Namespace: "orders", Version: v, OpenAPISpec: spec}},
	})
}
```

One registration with both `Service` entries also works
([services.md](./services.md#two-versions-in-one-binary)); a drain then
takes both lines. TypeScript and Python services set `version` per
service ([registration-clients.md](./registration-clients.md)).

Generate each client from its version's snapshot, not from the live
service, so a client build never picks up an unreviewed change:

- **Go**: `cmd/gen-go-client` runs protoc-gen-go's generator (the
  version the root `go.mod` pins) on `api/orders/vN/proto.json`. No
  protoc. Calls use `conn.Invoke` on the gateway's gRPC ingress; no
  service stubs are needed.
- **TypeScript**: `openapi-typescript` (pinned in
  `ts/package.json`) on `api/orders/vN/openapi.json`, calls with
  `openapi-fetch` on `/api/ingress`.

`check.sh` regenerates both and fails when they differ from the
commit.

## Retiring v1

The order is: deprecate, measure, declare, move, retire, delete.
Output below is from `walkthrough.sh`.

**1. Deprecate.** v1 gets `@deprecated(reason: "v2 is current")` as soon
as v2 registers. Add the date with the admin API, so every generated
GraphQL client shows it:

```bash
curl -X POST $GW/api/admin/services/orders/v1/deprecate -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"reason":"orders v1 ends 2027-06-30; move to v2 (docs/versioning.md)"}'
```

```
v1: OrdersV1QueryNamespace! @deprecated(reason: "orders v1 ends 2027-06-30; move to v2 (docs/versioning.md)")
```

**2. Measure callers.** The gateway counts calls per version, method
and caller. A caller id needs a source: `gwag up --caller-id-public`
(the `X-Caller-Id` header, or `caller-id` gRPC metadata; forgeable),
`WithCallerHeaders`, or the HMAC and delegated extractors
([caller-identity.md](./caller-identity.md)). Without one every caller
is `unknown`. The Connect ingress does not pass headers to the
extractor; use the gRPC ingress or GraphQL / REST for callers you need
to tell apart.

```
GET /api/admin/services/deprecated/stats?window=1h
orders/v1 GET /v1/orders/{id} old-ts-client 2
orders/v1 GET /v1/orders/{id} old-go-client 1
orders/v1 GET /v1/orders old-go-client 1
```

`GET /api/admin/services/usage` gives the same for every version.
Stats are per node and in memory.

**3. Declare dependents.** A service registered with gwag declares the
lines it calls in `depends`. A consumer the gateway cannot see is an
external dependent with an owner and a review date
([lifecycle.md](./lifecycle.md#external-dependents)). The example's
billing service (TypeScript) depends on orders v1; Acme's mobile app is
declared:

```ts
services: [{ namespace: "billing", version: "v1", openapiSpec,
  depends: [{ service: "orders", lines: ["v1"] }] }]
```

**4. The sunset rule refuses.** A line is sunset-able when every
dependent that accepts it accepts another line with a ready instance
and it had no traffic in the window. While billing and Acme pin v1:

```
GET /api/admin/graph?window=1m
orders/v1 sunsetable=false: 5 calls in the last 1m on this node; billing/v1 accepts orders [v1]: no other ready line; external acme (Acme mobile app) accepts orders [v1]: no other ready line

$ gwag ctl drain orders-v1@http://localhost:18621
283d3196deeae5b6f145d231 orders-v1@http://localhost:18621 blocked inflight=0 lines=orders/v1
  blocked: billing/v1 accepts orders [v1]: no other ready instance
  blocked: external acme (Acme mobile app) accepts orders [v1]: no other ready instance
```

**5. Move the callers.** billing ships a build that calls v2 and
declares `lines: ["v2"]`; Acme's declaration is updated when its
release is out. The drain then completes, and after a minute without
v1 traffic the line is sunset-able:

```
283d3196deeae5b6f145d231 orders-v1@http://localhost:18621 safe_to_stop inflight=0 lines=orders/v1
orders/v1 sunsetable=true: every dependent has another ready line; no traffic in the last 1m on this node
```

A deploy pipeline can read `sunsetable` from `/admin/graph` (or the
`line_sunsetable` metric) and refuse the build that drops v1 until it
is true.

**6. Retire.** Deploy the build without v1. Its v1 registration is
gone, the schema rebuilds without `v1`, and an old client gets
`Unimplemented` (gRPC) or 404 (REST).

**7. Delete the code.** v1 is a set of files, by construction:

```
internal/api/v1  api/orders/v1  testdata/contract/v1  cmd/legacy
internal/cases   clients/go/ordersv1  cmd/old-client
ts/src/gen/orders-v1.ts  ts/src/old-client.ts
```

plus the v1 lines in `internal/server/server.go` and
`./snapshot.sh update --allow-breaking` for the GraphQL snapshot.

## Events and channels

An event channel belongs to one proto package (`orders.v1.OrderEvents`,
[pubsub.md](./pubsub.md#named-channels)), so a breaking change to an
event is a new channel in the v2 package. Two orders of work:

- **Dual publish.** The producer publishes each domain change on both
  the v1 and the v2 channel, mapping from the domain as the request
  adapters do, until no consumer listens on v1. Consumers declare
  `subscribes`, which counts in the sunset rule like `depends`: the
  v1 line is not sunset-able while a subscriber needs its channel.
- **Consumers first.** Consumers learn to handle both messages, then
  the producer switches to v2 in one step. Fewer moving parts when you
  own every consumer.

With Kafka and a Schema Registry, the subject's compatibility level
decides which changes can stay on one subject; see
[events-kafka.md](./events-kafka.md).

## Proto and gRPC services

The same shape works without gat: two proto packages, an adapter
server for the old one, both registered on one `grpc.Server`. Not in
the repo as a runnable example.

```proto
// orders/v1/orders.proto: frozen. Never reuse a field number.
package orders.v1;
message Order {
  string id = 1;
  double amount = 2;
  string customer_name = 3;
  reserved 4;                 // removed field
  reserved "discount_code";
}
```

```go
package v1adapter

// Server serves orders.v1 over the domain. It holds no logic.
type Server struct {
	ordersv1.UnimplementedOrdersServiceServer
	Store *domain.Store
}

func (s *Server) GetOrder(ctx context.Context, req *ordersv1.GetOrderRequest) (*ordersv1.Order, error) {
	o, err := s.Store.Get(req.GetId())
	if err != nil {
		return nil, status.Error(codes.NotFound, "order "+req.GetId()+" not found")
	}
	if o.Currency != "USD" {
		return nil, status.Errorf(codes.FailedPrecondition, "order %s is in %s; use orders.v2", o.ID, o.Currency)
	}
	return &ordersv1.Order{Id: o.ID, Amount: float64(o.AmountCents) / 100, CustomerName: o.Customer}, nil
}
```

```go
srv := grpc.NewServer()
ordersv2.RegisterOrdersServiceServer(srv, &v2server.Server{Store: store})
ordersv1.RegisterOrdersServiceServer(srv, &v1adapter.Server{Store: store})

controlclient.SelfRegister(ctx, controlclient.Options{
	ServiceAddr: "orders:50051",
	Services: []controlclient.Service{
		{Namespace: "orders", Version: "v1", ProtoSource: ordersV1Proto},
		{Namespace: "orders", Version: "v2", ProtoSource: ordersV2Proto},
	},
})
```

Freeze `orders/v1/orders.proto` with
`gwag ctl schema snapshot check --spec orders/v1/orders.proto --snapshot api/orders/v1/proto.json --format proto`.
Contract tests are HTTP only today; for gRPC, table tests over the
adapter's methods do the same job.

## Run the example

```bash
examples/versioning/check.sh          # tests, goldens, snapshots, client drift; no gateway
examples/versioning/walkthrough.sh    # the retirement above against bin/gwag up, ~2 min
```
