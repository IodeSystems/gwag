# gwag as a Schema Registry

gwag can serve the Confluent Schema Registry REST API, so Kafka
serdes, Kafka Connect and registry UIs that talk to a Confluent
registry can talk to gwag instead. Use it in two steps:

1. **Connect.** Keep the corp registry. gwag's serdes, `gwag schema
   push`, `gwag schema check` and `gwag gen --registry` work against it
   (own, import, shadow: see [events-kafka.md](./events-kafka.md)).
2. **Replace.** Turn on the registry in gwag, copy the corp registry
   into it with `gwag schema import`, and point clients at gwag. Every
   record on the topics carries a schema id; the import keeps the ids,
   so records written before the switch still decode.

The API is read-only by default. Status: experimental.

## Turn it on

```
gwag up --schema-registry
```

Clients use `http://HOST:8080/schema-registry` as the registry URL. The
Go, TypeScript and Python serdes, Confluent's Java and Python clients
accept a URL with a path.

| Flag | Default | |
|---|---|---|
| `--schema-registry` | off | mount the API at `/schema-registry` |
| `--schema-registry-writes` | off | allow writes: dev and admin tokens and app keys register and delete schemas, the admin token sets compatibility levels |
| `--schema-registry-strategy` | `topic` | subject of a channel's message: `topic` (`<topic>-value`), `record` (`<message>`), `topic-record` (`<topic>-<message>`) |
| `--schema-registry-topic KEY=TOPIC` | `EV_<pkg>_<Name>` | Kafka topic of a channel (repeatable) |
| `--schema-registry-first-id N` | 1 | lowest id gwag allocates |

`gwag serve --schema-registry` takes the same flags (in-memory store).
In the library: `gateway.WithSchemaRegistry(gateway.SchemaRegistryOptions{...})`
and mount `g.SchemaRegistryHandler()`:

```go
mux.Handle("/schema-registry/", http.StripPrefix("/schema-registry", g.SchemaRegistryHandler()))
```

## Storage

- **Cluster** (`gwag up`, `WithCluster`): JetStream KV bucket
  `gwag-schemas`, raised to the cluster replica policy with the other
  buckets (`--cluster-replicas`). Ids, versions and levels survive
  restarts and are the same on every node. Ids come from a
  compare-and-set counter, so nodes registering at once never hand out
  one id twice. Until the node has joined JetStream, requests get
  `50003`.
- **Single gateway, no cluster**: memory. Ids are lost on restart; use
  it for tests and `gwag serve`.

## Channels gwag owns

Every named channel of a registered proto slot becomes a subject when
the slot registers: `<topic>-value` with the default strategy, topic
`EV_<pkg>_<Name>` (the Kafka backend's default) unless
`--schema-registry-topic` maps it. The schema is the message's file as
`schemaregistry.SchemaOf` renders it, imports registered under their
import paths as references: the bytes the Go serde's `AutoRegister`
sends. A serde in lookup mode (the default) finds the id gwag
allocated. A changed file adds a version.

## Control plane schema

The registry always holds the control plane's schema as the subject
`gateway.controlplane.v1.ControlPlane` (the gRPC service name):

```
curl http://HOST:8080/schema-registry/subjects/gateway.controlplane.v1.ControlPlane/versions/latest
```

It is `gw/proto/controlplane/v1/control.proto`, comments included, and
changes with the gateway version. A service generates its registration
client from it: [`registration-clients.md`](./registration-clients.md).

Its id is 2147483647. gwag allocates ids for its own schemas from the top
of the int32 range downward, apart from the counter for channels and
imports, which counts up from 1. An import never collides with it, and
`--schema-registry-first-id` does not apply to it. The subject is a
service name, so it cannot be a channel subject (a topic name or a
message name).

## Import

```
gwag schema import --from https://sr.corp.example:8081
```

- Reads every subject, version, schema id (both formats) and
  compatibility level from `--from` and sends them to the gateway's
  `POST /schema-registry/gwag/import` with the admin token. Source
  credentials: `GWAG_REGISTRY_USERNAME` / `GWAG_REGISTRY_PASSWORD`,
  `GWAG_REGISTRY_TOKEN`, `GWAG_REGISTRY_CA`, or user info in the URL.
- `--to URL` (default: the `.gw` primary endpoint + `/schema-registry`),
  `--bearer TOKEN` (default: from `.gw`), `--subject S` (repeatable; the
  subjects its references name come too), `--include-deleted`
  (soft-deleted versions stay deleted), `--dry-run`.
- Ids and version numbers are kept. New ids are allocated above the
  highest id gwag holds.
- Run it again to pick up new versions; what gwag already holds is
  left as it is.
- An id or subject version gwag holds with a different schema fails
  the whole import (`40901`), naming each conflict. Nothing is written.

gwag registers its own channels at boot, before an import can run, and
those ids start at 1: they collide with the corp registry's ids. Set
`--schema-registry-first-id` above the corp registry's highest id
before the first boot, or import before registering services with
named channels.

## Auth and writes

- Reads need no token, like the schema export (`/api/schema/*`).
- Writes (`POST /subjects/{s}/versions`, `DELETE /subjects/...`,
  `PUT /config`) answer `42205` (read-only) unless
  `--schema-registry-writes`. With it, the token goes as the basic-auth
  password (the user name is ignored; Confluent clients send
  `basic.auth.user.info=x:TOKEN`) or as a bearer:

  | Token | Register, delete | `PUT /config`, `/gwag/import` |
  |---|---|---|
  | admin | every subject | yes |
  | dev | every subject | `40301` |
  | app key | the subjects of its namespaces' channels | `40301` |
  | none, or an unknown, expired or revoked key | `40101` | `40101` |

- An app key's subjects are, for every named channel a slot of one of
  its namespaces declares, the subject of each channel message (by the
  strategy above) and the subjects of the files those messages import
  (by import path, well-known files excluded). An import file two
  namespaces share, such as `gw/proto/ps/v1/options.proto`, belongs to
  both. Other subjects, such as ones imported from the corp registry,
  need the dev or admin token (`40301`). The namespace's slot must be
  registered for its channels to count. App keys work here whether or
  not the gateway runs with `--require-app-keys`. See
  [services.md](./services.md#app-keys).
- `GET /mode` answers `READONLY` or `READWRITE`.
- The import endpoint needs the admin token, writes on or off.

## Deletes

As in Confluent, a delete is soft first:

- `DELETE /subjects/{s}` soft-deletes every live version and answers
  the version numbers (`[1,2]`). The versions stay readable with
  `?deleted=true`. Again on a soft-deleted subject: `40404`.
- `DELETE /subjects/{s}?permanent=true` removes the soft-deleted
  versions, the subject and its compatibility level. On a subject with
  live versions: `40405`.
- `DELETE /subjects/{s}/versions/{v|latest}` soft-deletes one version
  and answers its number; `?permanent=true` removes it. A soft-deleted
  version deleted soft again: `40406`; a live version deleted
  permanently: `40407`.
- A delete is refused with `42206` while any schema, in a live or a
  soft-deleted version, references the version (`references` naming
  that subject and version), for every token, admin included. Delete
  the referrer first, permanently.
- A permanent delete also drops a schema id no subject version holds
  any more: `GET /schemas/ids/{id}` then answers `40403`. Ids are not
  reused. The next registration in a subject takes the number after
  its highest remaining version, so a permanently deleted number can
  come back.
- gwag registers its channels' subjects again only when a channel's
  file changes or the gateway restarts; a deleted channel subject
  stays deleted until then.

## Endpoints

Content type `application/vnd.schemaregistry.v1+json`; errors are
`{"error_code", "message"}` with Confluent's codes (`40401` subject,
`40402` version, `40403` schema not found; `40404` / `40406` subject /
version soft-deleted; `40405` / `40407` subject / version not
soft-deleted first; `40408` no subject-level config; `42201` invalid
schema; `42202` invalid version; `42203` invalid level; `42205`
read-only; `42206` referenced; `40101` no valid token; `40301` token
not allowed; `50003` store not ready).

| Route | |
|---|---|
| `GET /schemas/types` | `JSON`, `PROTOBUF`, `AVRO` |
| `GET /schemas/ids/{id}` | `?format=serialized` gives base64 of the FileDescriptorProto, the default is `.proto` text |
| `GET /schemas/ids/{id}/schema`, `/versions`, `/subjects` | |
| `GET /subjects` | `?deleted`, `?subjectPrefix` |
| `GET /subjects/{s}/versions` | `?deleted` |
| `GET /subjects/{s}/versions/{v\|latest}` | `?format`, `?deleted` |
| `GET /subjects/{s}/versions/{v}/schema`, `/referencedby` | |
| `POST /subjects/{s}` | lookup: the version holding the schema; never registers |
| `POST /subjects/{s}/versions` | register (writes only) |
| `DELETE /subjects/{s}` | soft, or `?permanent=true` (writes only) |
| `DELETE /subjects/{s}/versions/{v\|latest}` | soft, or `?permanent=true` (writes only) |
| `GET /config`, `GET /config/{s}` | `?defaultToGlobal`; global default `BACKWARD` |
| `PUT /config`, `PUT /config/{s}` | writes only |
| `GET /mode`, `GET /mode/{s}` | |
| `POST /compatibility/subjects/{s}/versions/{v\|latest}` | `?verbose` |
| `POST /compatibility/subjects/{s}/versions` | against the versions the level names |
| `GET /contexts` | `["."]` |
| `POST /gwag/import` | admin token; body from `gwag schema import` |

Not implemented: `DELETE /config`, `PUT /mode`, schema contexts other than `.`, exporters, and
compatibility checks of AVRO and JSON schemas (identical text only).

## Matching schemas

A lookup or registration matches a stored schema by type, references
and either identical text or the same compiled file. The compiled
comparison ignores the file name, source info, `json_name`,
`proto3_optional` on extensions and custom options: Confluent's
`.proto` text leaves custom options out, so a schema read back from a
Confluent registry has none. A lookup answers with the version that has
identical text, else the oldest equivalent version, as Confluent does.

## Compatibility

`POST /compatibility/...` uses the subject's level (else the global
one):

| Level | Check |
|---|---|
| `NONE` | always compatible |
| `BACKWARD`, `FULL` | every field number both versions declare keeps its type, cardinality and shared enum values; a message the old version has is still there |
| `FORWARD` | the field check only |
| `*_TRANSITIVE` | the same against every live version when the request names no version |

Adding or removing fields is compatible (protobuf skips unknown
fields). The field check is `schemaregistry.CompareShared`, the code
behind `gwag schema check`.

## Verified with

`examples/events/sr-replace.sh` (docker): records written through a
Confluent registry by the Go, Python and TypeScript serdes and by
Confluent's Python and Java serializers; `gwag schema import`; the
Confluent registry stopped; then the gwag serdes and Confluent's Python
`ProtobufDeserializer` and Java `kafka-protobuf-console-consumer` read
them through gwag with the same ids, and the gwag serdes write new
records in lookup and latest mode with the imported ids. Kafka Connect
and registry UIs are not tested.
