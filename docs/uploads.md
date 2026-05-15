# File uploads

gwag accepts file uploads through one unified GraphQL surface — the
`Upload` scalar — fed by two complementary wire shapes:

| Wire shape | Best for | Endpoint |
|---|---|---|
| graphql-multipart-request-spec | Small inline files in the same request | `POST /api/graphql` (or wherever you mount `gw.Handler()`) |
| tus.io v1.0 | Large / resumable / behind-edge-LB files | `POST /api/uploads/tus` (mount via `gw.UploadsTusHandler()`) |

Either form arrives at the resolver as a `*gw.Upload`; the dispatcher
forwards it upstream the same way regardless of how it got there.

## The contract per ingest format

`Upload` is a public-schema scalar; each ingest format declares
"this argument is an Upload" in its native way and the IR
normalises to the same `ScalarUpload` token.

| Ingest format | Marker for "this is an upload" |
|---|---|
| **OpenAPI** | Operation with `requestBody.content["multipart/form-data"]` and a property whose schema is `type: string, format: binary` (or `format: byte`). Arrays of binary props produce `[Upload!]!`. Other form-data properties become regular scalar args. |
| **Proto** | A `bytes` field marked `[(gwag.upload.v1.upload) = true]` (extension at `gwag/upload/v1/options.proto`). The dispatcher reads the upload body into the field, capped by `WithUploadLimit`. See "Proto bytes binding" below. |
| **Downstream GraphQL** (`AddGraphQL`) | If the upstream service already declares its own `Upload` scalar, the mirror passes it through as `Upload`. |

The same operation, declared either way, shows up in the gateway's
client-facing SDL identically:

```graphql
mutation($file: Upload!) {
  files {
    upload(file: $file) { id size }
  }
}
```

## Wire shape 1: graphql-multipart-request-spec (small files)

[graphql-multipart-request-spec][gmrs] is the de-facto standard for
GraphQL clients (Apollo, urql, graphql-request) when uploading inline.
The client sends a `multipart/form-data` POST with three kinds of
parts:

- `operations` (JSON): the usual `{query, variables, operationName}`
  with `null` placeholders where files go.
- `map` (JSON): a `{fileKey: [variablePath]}` mapping.
- One file part per `map` entry.

Example (single-file upload):

```
POST /api/graphql
Content-Type: multipart/form-data; boundary=----X

------X
Content-Disposition: form-data; name="operations"

{"query":"mutation($file: Upload!){ files { upload(file:$file){ id } } }","variables":{"file":null}}
------X
Content-Disposition: form-data; name="map"

{"0":["variables.file"]}
------X
Content-Disposition: form-data; name="0"; filename="hello.txt"
Content-Type: text/plain

hello world
------X--
```

Clients (`apollo-upload-client`, `graphql-request`, etc.) build this
for you — declare the variable as `Upload!` and pass a `File` /
`Blob` and the library does the encoding.

**Batched `operations` (array form) is rejected** with `HTTP 400` and
a GraphQL `errors` envelope. The spec allows it; gwag does not. If
you need it, file an issue with a real use case.

## Wire shape 2: tus.io (large / resumable files)

[tus.io v1.0][tus] is the right answer when uploads need to:

- exceed a reverse proxy / load-balancer's request-body cap (Cloudflare
  free tier 100 MiB, nginx default 1 MiB, AWS ALB 1 MiB),
- survive a flaky connection (resume from the last successful chunk),
- happen out-of-band so the GraphQL mutation lands fast.

Flow:

1. Client `POST`s to `/api/uploads/tus` with `Upload-Length: <bytes>`
   (or `Upload-Defer-Length: 1`) and optional `Upload-Metadata`
   (filename, content-type as base64 key-value pairs). Server replies
   `201 Created` with `Location: /api/uploads/tus/<id>`.
2. Client `PATCH`es chunks of `Content-Type: application/offset+octet-stream`
   to the upload URL with `Upload-Offset: <bytes>`. Server replies
   `204 No Content` with the new `Upload-Offset`. Mismatches return
   `409 Conflict` with the server's real offset so the client can
   resync.
3. Client sends a regular GraphQL mutation with the upload-id as a
   string variable:

   ```graphql
   mutation($file: Upload!) {
     files { upload(file: $file) { id } }
   }
   ```

   ```json
   { "variables": { "file": "<upload-id-from-step-1>" } }
   ```

4. Gateway's `Upload` scalar `ParseValue` accepts the string,
   wraps it in `*Upload{TusID: …}`. At dispatch time the dispatcher
   opens the assembled body from the configured `UploadStore` and
   forwards it upstream as `multipart/form-data`.

`HEAD /api/uploads/tus/<id>` reports the current `Upload-Offset` and
`Upload-Length`; `DELETE` removes an abandoned upload. `OPTIONS`
advertises supported extensions (`creation`, `creation-defer-length`,
`termination`) and the configured `Tus-Max-Size`.

[tus-js-client][tjc] is the canonical browser / node tus client; it
handles chunking, retry, and resume. Point it at your `/api/uploads/tus`
endpoint and feed the resulting upload-id into your GraphQL variables.

Authentication: tus endpoints are **public by design** — the upload
id is cryptographically random (16 bytes hex) and acts as the
credential. Wrap `UploadsTusHandler()` with bearer-level auth if you
need it.

## Proto bytes binding

For proto-ingested services, mark a `bytes` field with the
`(gwag.upload.v1.upload)` field extension and the gateway exposes
it as a GraphQL `Upload` arg instead of `String` (the default
mapping for `bytes`):

```proto
syntax = "proto3";
package files.v1;

import "gwag/upload/v1/options.proto";

service FilesService {
  rpc Upload(UploadRequest) returns (UploadResponse);
}

message UploadRequest {
  bytes data = 1 [(gwag.upload.v1.upload) = true];
  string filename = 2;
}
```

The extension is published from `gw/proto/upload/v1/options.proto`
in the gwag repository. Vendor it into your protos directory or
fetch it via your proto module manager; protocompile resolves it
through the same import path the file declares.

At dispatch time the proto dispatcher accepts both wire shapes
exposed by the `Upload` scalar — inline graphql-multipart-spec
(`*Upload` with body bytes already in memory) and tus-staged
(`*Upload{TusID: …}` materialised from `WithUploadStore`) — reads
the body capped by `WithUploadLimit`, and writes the bytes into the
proto field via dynamicpb. The dispatcher always reads the upload
fully into memory before the gRPC call (gRPC unary needs a complete
request), so the cap is what bounds memory.

Limitations:
- One extension value per field — `true` opts in; `false` or
  omitting the option leaves the field as plain `bytes` (`String`
  on the GraphQL side).
- The extension is only meaningful on `bytes` fields. Setting it on
  any other kind is a no-op.
- Filename / content-type metadata don't ride through to the proto
  message — there's no convention for "the field that carries the
  upload's filename." If your upstream needs it, declare a sibling
  `string filename` field and your client passes it explicitly.
- Repeated `bytes` (`bytes data = 1 [(gwag.upload.v1.upload) =
  true];` with `repeated`) is not yet supported. Pull when an
  adopter needs multi-file proto uploads.

## Gateway configuration

```go
gw := gateway.New(
    // Pick ONE of these:
    gateway.WithUploadDataDir("/var/lib/gwag/uploads"),      // default fs store
    gateway.WithUploadStore(myCustomStore),                  // custom impl

    // Optional cap; 0 = unlimited at the gateway layer.
    gateway.WithUploadLimit(100 << 20),                      // 100 MiB
)

mux := http.NewServeMux()
mux.Handle("/api/graphql", gw.Handler())
mux.Handle("/api/uploads/tus", gw.UploadsTusHandler())          // collection
mux.Handle("/api/uploads/tus/", gw.UploadsTusHandler())         // resource
```

### `WithUploadLimit(int64)`

Per-upload byte cap, enforced at both the inline multipart parser
(via `http.MaxBytesReader` on the request body) and the tus PATCH
path (via the store's declared-length / streaming check). Set this
alongside your reverse-proxy / LB body-size limit so oversized
uploads fail fast at the right layer.

### `WithUploadDataDir(string)`

Installs the default `FilesystemUploadStore` at the given directory
with a 24-hour TTL on staged uploads. The gateway creates the store
on `New()` and closes it on `Close()`.

### `WithUploadStore(UploadStore)`

Plug a custom `UploadStore` implementation — useful when running a
multi-node cluster against shared storage (S3, GCS, Postgres LO).
The interface is small: `Create`, `Append`, `Info`, `Open`, `Delete`.
See `gw/upload_store.go` for the contract.

### Outbound

When the gateway dispatches an Upload-typed argument upstream:

- **OpenAPI services**: forwarded as `multipart/form-data` with the
  client's `Filename` + `Content-Type` preserved on the part. The
  dispatcher streams from the `UploadStore` so memory stays bounded
  even on large uploads.
- **Proto services**: deferred. A `[(gwag.upload) = true]` field
  extension on a `bytes` field with dispatcher-side `Upload.Open`
  is the sketched shape; pulled in by adopter demand.

## Error shapes

| Failure | Shape |
|---|---|
| Multipart parse error (missing `operations` part, invalid `map`) | HTTP 400 + GraphQL `errors` envelope |
| Batched `operations` array | HTTP 400 + `errors[0].message = "batched operations not supported"` |
| Upload exceeds `WithUploadLimit` (inline path) | HTTP 400/413 + `errors` envelope |
| tus offset mismatch | HTTP 409 + `Upload-Offset` reporting server's real offset |
| tus `Upload-Length` overshoot | HTTP 413 + `Upload-Offset` reporting trimmed length |
| tus version mismatch (`Tus-Resumable`) | HTTP 412 |
| tus PATCH without `Content-Type: application/offset+octet-stream` | HTTP 415 |
| No `UploadStore` configured | HTTP 503 from tus endpoints; `Upload.Open` error from dispatcher |
| Mutation references unknown tus upload-id | GraphQL error: `Upload.Open: tus id "<id>": upload: not found` |

## Implementation notes

- The `Upload` scalar is always present in SDL — your codegen sees
  `scalar Upload` whether or not any registered service uses uploads,
  so adopters can plan around it.
- `*Upload.File` is only set on the inline path. tus-staged uploads
  set `TusID` and lazy-open via `(*Upload).Open(ctx, store)` at
  dispatch time. Resolvers / custom dispatchers should always go
  through `Open()` so both wire shapes work.
- The inline multipart parser uses `multipart.ReadForm` with a 32 MiB
  in-memory threshold (spilling to tempfile beyond). Streaming the
  full part body to the configured `UploadStore` (mirroring tus) is
  a v1.1 follow-up; until then, `WithUploadLimit` is the right
  control for inline uploads.

[gmrs]: https://github.com/jaydenseric/graphql-multipart-request-spec
[tus]: https://tus.io
[tjc]: https://github.com/tus/tus-js-client
