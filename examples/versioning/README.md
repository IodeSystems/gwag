# Two API versions in one service

An `orders` service on gat that serves a frozen v1 next to v2 from one
process, for clients that cannot move yet. The guide is
[`docs/versioning.md`](../../docs/versioning.md).

```
internal/domain/        Order, Store: belongs to no version
internal/api/v2/        v2 handlers: wire types <-> domain
internal/api/v1/        v1 adapter: wire types <-> domain, mapping rules, applyDiscount + sunset
internal/server/        one mux, one huma API per version, one gat gateway; tests
cmd/orders/             serve; -gateway registers v1 and v2 with gwag; -no-v1
cmd/legacy/             the old v1-only implementation; records goldens, never deployed
cmd/specs/              writes the generated specs, in process
cmd/gen-go-client/      Go messages from a proto snapshot (protoc-gen-go's generator, no protoc)
cmd/old-client/         Go client on v1 (gRPC ingress)
cmd/new-client/         Go client on v2
api/orders/             frozen snapshots: vN/openapi.json, vN/proto.json, schema.graphql
testdata/contract/v1/   v1 goldens recorded from cmd/legacy
clients/go/             generated, from api/orders/vN/proto.json
ts/                     TypeScript clients (generated from api/orders/vN/openapi.json) and billing, a service that depends on orders
```

```bash
./check.sh          # tests, goldens vs legacy, snapshots, client drift; no gateway
./walkthrough.sh    # deprecate, measure, refuse, move, retire v1 against bin/gwag up (~2 min)
./snapshot.sh update   # after a reviewed spec change
```

Run by hand:

```bash
bin/gwag up --caller-id-public                       # from the repo root
go run ./examples/versioning/cmd/orders -gateway localhost:50090
go run ./examples/versioning/cmd/old-client -grpc localhost:50090
go run ./examples/versioning/cmd/new-client -grpc localhost:50090
```
