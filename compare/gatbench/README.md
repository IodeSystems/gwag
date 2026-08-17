# gatbench — what gat's abstraction costs

`compare/` next door compares gwag against other *gateways*, process to
process. This directory answers the other question: **what does gat
cost against writing each surface by hand?**

gat's pitch is one huma registration producing REST, GraphQL and gRPC.
The alternative is writing them separately — gqlgen for GraphQL,
connect-go for gRPC, grpc-gateway for REST-over-proto. Those are the
baselines here.

Current numbers: [`results.md`](./results.md).

## Why a separate Go module

gat's selling point is a small dependency tree — ~250 modules against
~498 for full gwag. Pulling gqlgen and grpc-gateway into the root
`go.mod` to benchmark against them would undercut the claim being
measured and land those deps in every downstream `go.sum`.

So `compare/gatbench` is its own module with a `replace` back to the
repo root. Nothing here is reachable from `go build ./...` at the
root, and `go test ./gw/...` never sees it.

## Layout

```
model/           the fixed workload — one Project type, 25 rows, no I/O
gatimpl/         two huma operations via gat.Register → REST + GraphQL + Connect
gqlgenimpl/      the same two operations as a hand-written gqlgen schema
connectimpl/     the same two as a proto service → connect-go + grpc-gateway
gatbench_test.go the cross-framework matrix
report.sh        run everything, render results.md + splice compare/README.md
```

Every implementation reads the same `model.Store` and returns the same
fields. Any difference in the numbers is framework cost.

## Method

Requests go through `http.Handler.ServeHTTP` against an
`httptest.ResponseRecorder`. No sockets, no TLS, no loopback — a
loopback round trip costs ~300-400µs on this class of host, which is
larger than most of the deltas being measured.

That makes the absolute ns/op smaller than anything you would see in
production. The ratios are the portable part.

Two shapes, because they stress different things:

- **Point lookup** — one object, three fields. Dominated by
  per-request fixed cost: parse, plan, bind, encode.
- **25-row list** — 75 leaf fields. Dominated by per-field executor
  cost.

## Run it

```bash
compare/gatbench/report.sh          # 5 counts, ~2 min, rewrites results.md
COUNT=1 compare/gatbench/report.sh  # quick pass
cd compare/gatbench && go test . -run '^$' -bench Get -benchmem
```

`report.sh` also rewrites the `<!-- BEGIN gat-matrix -->` block in
`compare/README.md`, so the headline table cannot drift from the numbers.

## Regenerating the fixtures

The gqlgen and proto code is generated and committed — don't edit it.

```bash
cd compare/gatbench/gqlgenimpl && go run github.com/99designs/gqlgen generate

# proto → Go, connect-go, grpc-gateway. Plugins land in compare/gatbench/.bin.
cd compare/gatbench
GOBIN=$PWD/.bin go install \
  connectrpc.com/connect/cmd/protoc-gen-connect-go@latest
GOBIN=$PWD/.bin go install \
  github.com/grpc-ecosystem/grpc-gateway/v2/protoc-gen-grpc-gateway@latest
GAPI=$(go env GOMODCACHE)/github.com/grpc-ecosystem/grpc-gateway@v1.16.0/third_party/googleapis
PATH="$PWD/.bin:../../.bin:$PATH" protoc \
  --proto_path=connectimpl/proto --proto_path=$GAPI \
  --go_out=. --go_opt=module=github.com/iodesystems/gwag/compare/gatbench \
  --go-grpc_out=. --go-grpc_opt=module=github.com/iodesystems/gwag/compare/gatbench \
  --connect-go_out=. --connect-go_opt=module=github.com/iodesystems/gwag/compare/gatbench \
  --grpc-gateway_out=. --grpc-gateway_opt=module=github.com/iodesystems/gwag/compare/gatbench \
  gatbench/v1/gatbench.proto
```

## Caveats

- **The competitors are not drop-in replacements for gat.** gqlgen
  gives you GraphQL and nothing else; you write the REST and gRPC
  surfaces yourself, and keep three schemas in sync by hand. The
  benchmark prices the runtime, not the maintenance.
- **grpc-gateway runs in-process here**
  (`RegisterProjectServiceHandlerServer`), not over a loopback gRPC
  dial. That is the fair shape against gat, which also dispatches
  in-process; the `FromEndpoint` variant would measure the loopback.
- **gqlgen is configured with its query cache on**, as its own
  recommended server does. Without it the comparison would be about
  cache configuration.
- **One host, one workload.** A different selection shape — deep
  nesting, large scalars, many aliases — will move these ratios.
