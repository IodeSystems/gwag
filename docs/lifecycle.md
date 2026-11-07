# Service lifecycle

Services don't just appear and vanish — they're version-tagged,
deprecated with warnings, and retired only after their callers move
off. The gateway's job is to make every step of that visible.

## Register

Services self-register over the gRPC control plane and heartbeat
to stay alive. One Register call can carry many services on one
address (multiple RPCs in one binary). Heartbeats every TTL/3;
missed heartbeats past TTL evict.

```go
import (
    _ "embed"

    "github.com/iodesystems/gwag/v2/controlclient"
)

//go:embed greeter.proto
var greeterProto []byte

reg, _ := controlclient.SelfRegister(ctx, controlclient.Options{
    GatewayAddr: "gateway:50090",
    ServiceAddr: "greeter:50051",
    Services: []controlclient.Service{
        {Namespace: "greeter", ProtoSource: greeterProto},
    },
})
defer reg.Close(ctx) // graceful deregister
```

Services ship raw `.proto` bytes; the gateway compiles them via
`protocompile` so leading / trailing comments survive into the
GraphQL SDL. Multi-file `.proto` layouts (one entrypoint with
`import "..."` statements) use `ProtoFS fs.FS` + `ProtoEntry string`
with any `fs.FS` (`embed.FS`, `os.DirFS(...)`, archives), or pass
the imports map explicitly via `ProtoImports map[string][]byte`.

OpenAPI services use the same `Service` struct with `OpenAPISpec`
instead of `ProtoSource`; same shape, both formats ship raw source.
The control-plane API is in
[`gw/proto/controlplane/v1/control.proto`](../gw/proto/controlplane/v1/control.proto).

## Version

A service can register at one of three **tiers** per namespace:

| Tier | What it is | Mutability | Deprecation |
|---|---|---|---|
| `unstable` | Trunk-tip; published on every push from CI | Always overwrites the prior `unstable` | Pinning to it lights up `@deprecated(reason: "unstable — pin to stable or vN for releases")` so codegen flags it in IDE/lint |
| `stable` | Alias to the most-recent numbered cut | Auto-rolls when a new `vN` is registered | Never deprecated |
| `vN` (`v1`, `v2`, …) | Pinned historical cuts | Immutable | Auto-`@deprecated` when a newer `vN` exists |

```graphql
type UserQuery {
  unstable: UserUnstableQuery @deprecated(reason: "unstable — pin to stable or vN for releases")
  stable:   UserV2Query        # alias to the latest cut (currently v2)
  v1:       UserV1Query @deprecated(reason: "use v2")
  v2:       UserV2Query
  # newest fields hoisted to the top
  profile(id: ID!): Profile     # from stable / v2
}
```

Flow:

1. **Trunk publishes to `unstable`.** Every CI green re-registers
   the service at `unstable`. The slot is mutable — no `vN` churn,
   no deprecated noise. Teams building against a dep can opt into
   its `unstable` for fast iteration.
2. **Cut a release.** Freeze `unstable` into a new `vN` and roll
   `stable` forward. Older `vN-1` and earlier flip to
   `@deprecated` automatically.
3. **Caller-side lint forces dependency negotiation.** Set
   `controlclient.Options.BuildTag` and the client refuses to
   register `unstable` from a release-tagged binary. A service that
   cuts a release must pin `stable` or a numbered `vN` on its deps
   — if a dep's `unstable` has diverged with breaking changes, you
   can't cut until that dep cuts (or you adopt the break).

Generated clients propagate `@deprecated` through normal codegen
channels: `protoc-gen-go` emits `// Deprecated: ...`,
graphql-codegen emits JSDoc `@deprecated` on TS hooks,
openapi-generator marks operations deprecated.

## Per-tier policy

Each gateway boots with `--allow-tier` controlling which tiers can
register against it:

```bash
# trunk-friendly gateway: anything goes
$ gwag --allow-tier unstable,stable,vN

# release-track gateway: no unstable
$ gwag --allow-tier stable,vN

# locked-down gateway: pinned cuts only (or stable+vN if you want evergreen)
$ gwag --allow-tier vN
```

A service that tries to register a disallowed tier is rejected at
the control plane with a clear error code. Operators who need
physical isolation between deployments (PCI/HIPAA, blast-radius
separation) run multiple gateways with distinct cluster names — that
side of the picture is a deployment concern, not something the
gateway needs to model.

## Deprecate

Two paths:

1. **Auto-deprecation on version fold.** Older `vN` get `@deprecated` automatically when newer cuts register; `unstable` carries a fixed deprecation reason any time it's queried. Free.
2. **Manual deprecate / undeprecate.** Operators flip a `deprecated` bit on any `(namespace, version)` from the admin UI or RPC, with a reason string. Useful for sunsetting a still-current `vN` ahead of cutting `vN+1`, or for un-deprecating in a rollback.

CI gate with `schema diff --strict`:

```bash
$ gwag schema diff \
    --from https://gw.internal/schema \
    --to   ./candidate.graphql --strict
```

`--strict` fails on changes that can break a working client query.
The rules:

- **Breaking** — exit non-zero: required arg / required input field
  removed, output field removed (any nullability), output field type
  changed, default value changed or removed, required arg / required
  input field added without a default, type / enum value removed.
- **Info** — printed but not a failure: optional arg / optional input
  field removed (callers who didn't pass it are unaffected; callers
  who did get a recoverable validation error), default value added,
  optional arg / field added, deprecation flipped on, new types /
  enums / fields.

The conservative "any removal is breaking" policy will relax once
caller-side usage tracking can answer "is anyone passing this
optional arg?" directly. Until then, the relaxation matches the
asymmetric reality that *adding* fields is always safe but
*removing* fields is mostly safe for the optional ones.

## Retire

Stop heartbeating; the gateway evicts after TTL. The schema
rebuilds; the field disappears.

### External dependents

A line is sunset-able (admin graph `sunsetable`, metric
`line_sunsetable`) when every dependent that accepts it also accepts
another line with a ready instance, and it had no traffic in the
window. Dependents are the `depends` of registered services. A
consumer the gateway cannot see (an API customer, a partner
integration, a mobile app release) is declared as an external
dependent:

```bash
curl -X PUT https://gw.internal/api/admin/external-dependents/acme \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"name":"Acme Corp","kind":"customer",
       "owner":{"team":"books","contact":"acme-api@example.com"},
       "depends":[{"service":"library","lines":["v1"]}],
       "note":"contract 2026-14","reviewAt":"2027-06-30T00:00:00Z"}'
```

It counts wherever a registered dependent counts: library v1 is not
sunset-able while Acme pins only v1, a drain of the last ready v1
instance is `blocked` with a reason naming Acme, and
`dependency_unmet{consumer_kind="external",consumer_namespace="acme"}`
is 1 while no pinned line has a ready instance. It does not affect the
start gate. `kind` is `customer`, `partner`, `app` or `other`; lines
follow the `depends` rule (`vN` or `unstable`). A pin on a service or
line that is not registered is accepted, with a warning in the
response. In a cluster the declarations live in the
`gwag-external-dependents` KV bucket and apply on every node. The
admin UI draws them on the dependency graph and edits them there.

#### Review date

`reviewAt` (RFC 3339) is required. A declaration pins lines for months
or years, so the gateway does not act on the date by itself: past
`reviewAt` the declaration keeps counting everywhere (sunset rule,
drain coverage, `dependency_unmet`, graph edges). It is listed with
`reviewDue: true`, is an item in `GET /admin/notifications` (the admin
UI's bell), and adds to the `declarations_review_due` gauge, until a
person reviews it. The response warns when `reviewAt` is in the past
or more than a year out; it still stores the declaration.

To review, confirm the pins with the owner and set the next date:

```bash
curl -X POST https://gw.internal/api/admin/external-dependents/acme/review \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"reviewAt":"2028-06-30T00:00:00Z","by":"ops@example.com"}'
```

The review op changes only `reviewAt`, `reviewedAt` (now) and
`reviewedBy` (the `by` field, self-reported and not checked); a new
`reviewAt` must be in the future. A PUT can also set a new date; it
keeps `reviewedAt` / `reviewedBy`. A declaration stored without a
review date is review-due. Delete it with
`DELETE /admin/external-dependents/acme`.

#### Notifications

`GET /admin/notifications` (GraphQL `admin.listNotifications`) lists
the items that need a person, oldest first:

```json
{"items": [{"kind": "declaration_review_due", "id": "review-due/acme",
  "title": "Review external dependent Acme Corp",
  "detail": "Review date 2027-06-30T00:00:00Z has passed. It still pins library [v1]. ...",
  "since": "2027-06-30T00:00:00Z", "href": "/?select=ext:acme"}]}
```

| `kind` | Item | Goes away |
|---|---|---|
| `declaration_review_due` | A declaration past `reviewAt`. Same on every node. | When it is reviewed or deleted. |
| `declaration_hook_failed` | A declarations hook delivery that ran out of retries. Only on the node that tried it; in memory, at most 20. | Full mode: when a later delivery succeeds. Delta mode: when the node restarts. |

More kinds may be added; a client shows an unknown kind by its
`title`, `detail` and `href`. `href` is an admin UI route.

#### Declaration sinks

Two optional sinks tell systems outside the gateway about every put,
review and delete. Each has a mode: `full` (the whole current set) or
`delta` (one change).

| Flag (`gwag up`, example gateway) | Library |
|---|---|
| `--declarations-hook URL` | `WithDeclarationsHook(DeclarationsHookOptions{URL, ...})` |
| `--declarations-hook-secret S` | `DeclarationsHookOptions.Secret` |
| `--declarations-hook-mode full\|delta` (default full) | `DeclarationsHookOptions.Mode` |
| `--declarations-file PATH` | `WithDeclarationsFile(DeclarationsFileOptions{Path, ...})` |
| `--declarations-file-mode full\|delta` (default full) | `DeclarationsFileOptions.Mode` |

**Hook.** A POST of one JSON payload per change, `Content-Type:
application/json`. The admin response does not wait for it: a worker
queue sends it after the declaration is stored. In a cluster only the
node that accepted the write sends it, so each change is POSTed once.
Headers:

| Header | Value |
|---|---|
| `X-Gwag-Event` | `put`, `review` or `delete` |
| `X-Gwag-Delivery` | Random id, the same on every retry of one change. Deduplicate on it. |
| `X-Gwag-Timestamp` | Unix seconds when this try was sent. |
| `X-Gwag-Signature` | With a secret: `sha256=` + hex(HMAC-SHA256(secret, timestamp + `.` + body)). The key is the secret's bytes as given. |

To verify: recompute the HMAC over the timestamp header, a `.` and the
raw body, compare in constant time, and reject a timestamp more than a
few minutes old (that blocks replay).

A 2xx answer is success. A network error, a 5xx, 408 or 429 is retried
with exponential backoff (500ms, doubling, at most 8s between tries,
6 tries in all; `DeclarationsHookOptions` changes these). Any other
4xx is not retried. When a change is given up, the node logs it,
increments `declarations_hook_failures_total` and lists a
`declaration_hook_failed` notification. The queue (256 changes) is in
memory: a delivery still pending when the accepting node stops is
lost, and a change that finds the queue full fails at once. In full
mode the next successful delivery carries the whole set; in delta mode
resync from `GET /admin/external-dependents`.

**File.** Every node writes its own file at PATH from the replicated
set, on every change it sees through the KV watch (standalone: on
every write). The directory must exist.

- `full`: the file is the full payload below, indented JSON. Each
  change replaces it: a temp file in the same directory, fsynced,
  mode 0644, renamed over PATH, so a reader sees the old file or the
  new one, never part of one. Written at start (standalone) or once
  the node has replayed the bucket (cluster). JSON, not YAML: it is
  the same document the hook sends, and needs no second schema.
- `delta`: one delta payload per line (JSON Lines), appended. A node
  writes no lines for the state it loads at start, only for changes
  after it; use full mode, or the `seq` gaps, to detect what a node
  missed while it was down. The gateway never rotates the file. It
  opens the file for each line, so an outside rotation that renames
  it (logrotate's default) works: the next change creates a new file.

**Payloads (stable).** `seq` orders changes: in a cluster it is the KV
revision of the change, the same on every node; standalone it counts
from 1 and restarts with the process, as the set does. A receiver keeps
the payload with the highest `seq`. Times are RFC 3339 UTC.

Full, `schema: "gwag.declarations.full/v1"`:

```json
{
  "schema": "gwag.declarations.full/v1",
  "seq": 42,
  "at": "2026-11-07T12:00:00.000Z",
  "declarations": [
    {
      "id": "acme",
      "name": "Acme Corp",
      "kind": "customer",
      "owner": {"team": "books", "contact": "acme-api@example.com"},
      "depends": [{"service": "library", "lines": ["v1"]}],
      "note": "contract 2026-14",
      "reviewAt": "2027-06-30T00:00:00Z",
      "reviewedAt": "",
      "reviewedBy": "",
      "updatedAt": "2026-11-07T12:00:00.000Z"
    }
  ]
}
```

`seq` is the revision the set is as of; `at` is when the payload was
built; `declarations` is sorted by `id` and never null. In a declaration
`owner` is `null` when none was given, and an empty string means unset
for the other string fields.

Delta, `schema: "gwag.declarations.delta/v1"`:

```json
{
  "schema": "gwag.declarations.delta/v1",
  "seq": 43,
  "op": "review",
  "id": "acme",
  "before": {"id": "acme", "reviewAt": "2027-06-30T00:00:00Z", "...": "..."},
  "after":  {"id": "acme", "reviewAt": "2028-06-30T00:00:00Z", "...": "..."},
  "at": "2027-07-01T09:30:00.000Z"
}
```

`op` is `put`, `review` or `delete`. `before` is `null` when the put
created the declaration; `after` is `null` on delete. Both have the
declaration shape above. A new field may be added to either schema; a
field is not removed or changed in meaning without a new `schema`
value.

## Promotion path

The tier flow above is the per-namespace story. Two extra gates wire
the same axis into CI:

1. **`services list`** exposes per-pool hashes — CI diffs hash sets
   across clusters to confirm the bytes match for every
   `(namespace, tier|version)` you intend to promote.
2. **`/schema` + `schema diff --strict`** is the client-perspective
   gate — additions are fine, removals or required-arg changes fail
   the build (rules in [Deprecate](#deprecate)).

Per-cluster drift is prevented by the canonical hash gate in the
pool: a replica with a mismatched proto can't join an existing
`(ns, version)` pool.

## What this *doesn't* (yet) tell you

The `dispatch_duration_seconds` Prometheus histogram already carries
a `caller` label (extracted via the caller-identity seams documented
in [`caller-identity.md`](./caller-identity.md) — forgeable-by-design
"public" mode, HMAC for production, delegated when you want richer
attrs alongside), so "who's calling this method?" is queryable today.

What's still missing is the *deprecated-operation* UI panel — a
per-(namespace, method) view of which callers are still hitting an
`@deprecated` op so you know who to nudge before retiring it. Until
that lands, deprecation is "announce, wait, retire," with the
schema-side warnings emitted automatically and Prometheus answering
"who's still on it?" via the caller label.
