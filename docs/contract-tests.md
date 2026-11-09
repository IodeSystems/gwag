# Contract tests

Golden request/response cases for one API version, replayed in process
against an `http.Handler`. Use them to keep a version's behavior fixed
while its implementation changes, for example `v1` rewritten as an
adapter over `v2`.

Package: `github.com/iodesystems/gwag/v2/gw/contract`. No dependencies
beyond the standard library.

## Case files

`testdata/v1/*.json`, one case or an array of cases per file, read in
file-name order:

```json
{
  "name": "get user",
  "method": "GET",
  "path": "/v1/users/1",
  "headers": {"Authorization": "Bearer x"},
  "ignore": ["/created_at"],
  "want": {"status": 200, "body": {"id": 1, "name": "ada", "created_at": "-"}}
}
```

- `method` defaults to `GET`. A `body` is sent as JSON unless a
  `Content-Type` header is given.
- JSON bodies compare ignoring object key order. Arrays compare in order.
  Numbers compare by text (`1` and `1.0` differ). A non-JSON body is a
  JSON string holding the exact text.
- `ignore` paths (`/a/b`, `/items/0/id`, `/items/*/id`) are dropped from
  both sides. Use them for ids and timestamps.
- `want.headers` are checked only when listed.
- A GraphQL case is a `POST` to the GraphQL path with
  `{"query": "...", "variables": {...}}` as the body, so the same helper
  covers a gat or gateway handler.

## Run

```go
func TestV1Contract(t *testing.T) {
    contract.Run(t, newV1Handler(), "testdata/v1")
}
```

One subtest per case. A failure lists each difference by path:

```
GET /v1/users/1 differs from the golden:
  status: want 200, have 404
  /name: missing, want "ada"
```

`go test -contract.update ./...` rewrites each case's `want` from the
handler under test and runs no assertions. Review the file diff before
committing; an update after a refactor of a frozen version means the
behavior changed.

## Record from the old service

Capture the goldens from the live, pre-rewrite service, then run them
against the adapter:

```go
err := contract.Record("http://old-service:8080", "testdata/v1", []contract.Case{
    {Name: "get user", Path: "/v1/users/1", Ignore: []string{"/created_at"}},
    {Name: "create user", Method: "POST", Path: "/v1/users", Body: json.RawMessage(`{"name":"ada"}`)},
})
```

`Record` writes `testdata/v1/<name>.json` per case. To record a response
header, list it in `Want.Headers` with any value.

## Limits

- Cases run in order of file name with no shared state. A case that
  depends on an earlier write needs a handler that seeds its own data.
- HTTP only. gRPC and subscriptions are not covered.
- Compare checks the whole body. It has no partial-match mode; use
  `ignore` to drop fields.
