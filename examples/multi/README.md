# examples/multi

Two unrelated services exposed by one gateway under their own
namespaces. Demonstrates the zero-config path: hand the library a list
of `.proto` files and gRPC destinations and you get a GraphQL surface.

## Run

```
$ cd examples/multi
$ go run .
2026/05/05 17:16:00 listening on :8080
```

In another terminal:

```
$ curl -sS -H 'Content-Type: application/json' \
       -d '{"query":"{ greeter { hello(name: \"world\") { greeting } } }"}' \
       http://localhost:8080/graphql
{"data":{"greeter":{"hello":{"greeting":"Hello, world!"}}}}

$ curl -sS -H 'Content-Type: application/json' \
       -d '{"query":"{ library { listBooks(author: \"\") { books { title author year } } } }"}' \
       http://localhost:8080/graphql
{"data":{"library":{"listBooks":{"books":[
  {"author":"Alan Donovan","title":"Go Programming","year":2015},
  {"author":"Brian Kernighan","title":"The Go Programming Language","year":2015},
  {"author":"Martin Kleppmann","title":"Designing Data-Intensive Applications","year":2017}
]}}}}
```

GraphiQL at <http://localhost:8080/graphql> (visit in a browser).

## CLI alternative

The same gateway shape, without writing Go: run the `go-api-gateway`
binary against the gRPC services directly. Standalone gRPC servers (not
bufconn) are required, since the binary takes `host:port` strings.

```
$ go install github.com/iodesystems/go-api-gateway/cmd/go-api-gateway@latest
$ go-api-gateway \
    --proto ./protos/greeter.proto=greeter-svc:50051 \
    --proto ./protos/library.proto=library-svc:50052 \
    --addr :8080
```

`PATH=[NAMESPACE@]ADDR`. Without a namespace, the proto's filename stem
is used. Dialing is insecure by default — fine inside a service mesh,
not the public network.
