# Rate limits, quotas and usage

This page is about one setup: a fleet of services calls an external,
rate-limited API (a vendor, a free tier) only through gwag. gwag
holds the vendor credential, keeps the whole fleet under the
vendor's limit across every gateway node, gives each calling service
its own quota, and reports who used how much.

The pieces work on any upstream kind (OpenAPI, downstream GraphQL,
gRPC, MCP). Status: experimental.

## Setup

Register the vendor API with its credential on the outbound client,
and the vendor's limit:

```go
type bearer struct {
    token string
    next  http.RoundTripper
}

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
    r = r.Clone(r.Context())
    r.Header.Set("Authorization", "Bearer "+b.token)
    return b.next.RoundTrip(r)
}

gw := gateway.New(
    gateway.WithCluster(cl),
    gateway.WithCallerIDHMAC(hmacOpts), // who is calling: see caller-identity.md
    gateway.WithUsage(gateway.UsageOptions{}),
)
err := gw.AddOpenAPI("./vendor-openapi.json",
    gateway.As("vendor"),
    gateway.To("https://api.vendor.example"),
    gateway.OpenAPIClient(&http.Client{Transport: bearer{token: os.Getenv("VENDOR_TOKEN"), next: http.DefaultTransport}}),
    gateway.ForwardHeaders(),                       // send no caller header to the vendor
    gateway.RateLimit(10, 10),                      // vendor: 10 requests per second
    gateway.OperationRateLimit("search", 2, 2),     // and 2/s on search
    gateway.OperationCost("search", 5),             // a search counts 5 units
)
```

The services in the fleet hold no vendor credential. They call
`vendor` through the gateway (GraphQL, REST, gRPC, MCP) with their
own caller id.

With `gwag up`, register the service through the control plane or
`gwag serve`, and set the limit as an operator instead:

```
gwag up --usage
gwag limit set --service vendor --rps 10 --burst 10
gwag limit set --service vendor --op search --rps 2 --burst 2
gwag cost set --service vendor:v1 --op search --units 5
```

## Rate limit

A rate limit is a token bucket on a scope:

| Scope | Covers |
|---|---|
| `--service ns` | every version and operation of `ns` together |
| `--service ns:vN` | every operation of that version together |
| `--service ns --op X` | operation `X` of every version together |
| `--service ns:vN --op X` | one operation of one version |

The operation name is the OpenAPI `operationId`, the proto method
name, the GraphQL field name, or the MCP tool name. A call takes one
token from every bucket whose scope covers it. A rate limit counts
calls; costs do not apply to it.

A call with no token available waits for one, up to the backpressure
`MaxWaitTime` (default 10 s; `WithBackpressure`). The wait smooths a
burst of callers into the vendor's rate. When the wait would be
longer, the call fails at once with `ResourceExhausted`: HTTP 429
with `Retry-After` (the time until a token is free, rounded up to
whole seconds), GraphQL `extensions.code: "resource_exhausted"` and
`retryAfterSeconds`, gRPC code 8. The call is not sent.

The wait runs in the caller's request on the limited service's own
bucket. A limited or waiting service does not delay calls to any
other service. A call that waits also holds its pool's backpressure
slot meanwhile, so `MaxInflight` of that pool bounds how many calls
can wait.

Set in code (`RateLimit`, `OperationRateLimit`) or by an operator:

```
gwag limit set --service vendor:v1 [--op X] --rps N --burst B
gwag limit delete --service vendor:v1 [--op X]
gwag limit list
```

An operator limit replaces a code limit for the same scope; deleting
it brings the code limit back. Operator limits are stored in the
`gwag-limits` bucket (in memory standalone); every node applies a
change within a second. Admin API: `PUT /admin/limits/rate`
`{service, operation, rps, burst}`, `DELETE /admin/limits/rate?service=&operation=`,
`GET /admin/limits` (dev role). Writes need the admin role.

### Several gateway nodes

Each node gets `rps / N` and `burst / N` (rounded down, at least 1),
where N is the live peer count. N is the larger of the peers bucket's
live set and this node's NATS routes; it is checked every second and
the buckets change at once. `rate_limit_split_peers` shows it.

How close this holds to the limit:

- Calls spread unevenly over the nodes get less than the limit, not
  more: a node with no traffic leaves its share unused.
- In any one second the cluster sends at most `rps + burst` (each
  node's bucket starts full). If the vendor counts in fixed 1-second
  windows and allows L, set `rps + burst <= L`, with some margin for
  network delay.
- A node that dies keeps its share for up to the peers TTL (30 s):
  the survivors stay at the smaller share until its entry expires.
- A node that joins takes its share as soon as it sees its routes;
  the others drop to the smaller share within a second.
- A burst below N lets the cluster burst up to N (one per node).
- During a network partition each side counts only the nodes it can
  reach. Both sides can then take more than their share.

The next step is leased blocks: a node takes a block of tokens from
a cluster counter and returns what it did not use, so idle nodes do
not hold capacity. Not built.

## Upstream 429

When a service with a rate limit at any scope answers 429, the
gateway pauses that service (namespace and version, all operations)
on every node:

- HTTP upstreams (OpenAPI, downstream GraphQL): status 429. The
  pause is the `Retry-After` header (seconds or an HTTP date).
  Without one, the default pause: 1 s, set with
  `WithUpstream429Pause(d)`; 0 means no pause without `Retry-After`.
- gRPC upstreams: `ResourceExhausted` with a `google.rpc.RetryInfo`
  detail; the pause is its `retry_delay`. `ResourceExhausted`
  without `RetryInfo` does not pause.
- A pause is at most 1 hour.

The caller of the call that got the 429 gets the upstream's 429 and
its `Retry-After`. During the pause, calls fail at once with 429 and
the remaining time as `Retry-After`; nothing is sent upstream. The
pause covers the service, not only the operation, because a vendor's
limit is usually per account.

The node that got the 429 publishes the pause on the NATS subject
`gwag.upstream.pause` with the remaining time, so clock differences
between nodes do not matter. A node that misses the message (it was
restarting) pauses when it gets its own 429. `gwag limit list` shows
the pauses in force on the answering node.

Services with no rate limit are not paused: their 429 goes to the
caller as before. An internal service that sends 429 for one user's
limit does not stop every other user.

## Quotas per caller

A quota limits one caller id over a fixed window. The caller id is
whatever caller-id mode the gateway runs
([`caller-identity.md`](./caller-identity.md)); without one every call
is `unknown`. There is no other credential for quotas.

```
gwag quota set --caller billing --service vendor --limit 10000 --per 1d
gwag quota set --default --service vendor --limit 1000 --per 1d
gwag quota set --default --service vendor --op search --limit 50 --per 1m --units
gwag quota list
gwag quota delete --caller billing --service vendor
```

- `--caller ID` sets one caller's quota; `--default` the quota of
  every caller without one of its own at that scope. Each caller
  counts separately against a default.
- Scopes are the rate-limit scopes, plus no `--service` for every
  service together. A call is checked against every scope that has a
  quota; at each scope the caller's own quota is used when there is
  one, else the default.
- `--per` is 1s to 24h (`1s`, `1m`, `1h`, `1d`, or a Go duration).
  Windows are fixed and start on a multiple of the window since the
  Unix epoch: a 1m window on the minute, 1d at 00:00 UTC.
- `--units` counts the operation's cost units instead of calls.

Over quota, the call fails with 429, `Retry-After` the time to the
window's end, and the message `quota exceeded for caller=ID ...`.
Calls refused by the rate limit after passing the quota are given
back to the quota.

Admin API: `PUT /admin/limits/quota` `{caller | default, service,
operation, limit, per, units}`, `DELETE /admin/limits/quota?caller=|default=true&service=&operation=`.

### Accuracy in a cluster

- Windows under 1 minute: each node allows `limit / N` (rounded down,
  at least 1) on its own. Exact on one node. In a cluster a caller
  whose calls land unevenly on the nodes gets less than the limit.
- Windows of 1 minute and more: one counter per (quota, caller,
  window) in the `gwag-quota-counters` bucket. Each node counts
  locally and adds its count to the cluster counter every flush
  interval (compare and set), then reads the total. A node refuses
  when the last total it read plus its own unflushed count reaches
  the limit. The cluster can go over by what the other nodes counted
  since their last flush: at most (N - 1) x (the caller's calls per
  node per flush interval). The flush interval is
  `UsageOptions.FlushInterval` (default 5 s).
- Counters of a past window are dropped on the node; the bucket
  keeps them 48 hours.

### The quota delegate

`WithQuota` (the `_quota_auth/v1` delegate,
[`caller-identity.md`](./caller-identity.md#quota-block-permit-pattern))
replaces built-in quotas. With `WithQuota` configured, quotas set
here are stored and listed (`quotaDelegate: true` in `GET
/admin/limits`) but not checked. Rate limits, pauses, costs and usage
work with either.

## Costs

`OperationCost("search", 5)` or `gwag cost set --service vendor:v1
--op search --units 5` makes one call count 5 units against
`--units` quotas and in the usage report. Default 1. An operator cost
replaces the code cost.

## Usage report

With `WithUsage(UsageOptions{})` (`gwag up --usage`) every node counts,
per (UTC day, caller, namespace, version, operation):

| Field | Counts |
|---|---|
| `calls` | every call that reached the gate, refused or not |
| `units` | cost units of the calls sent upstream |
| `rateLimited` | refused by the rate limit |
| `quotaExceeded` | refused by a quota |
| `upstream429` | the upstream answered 429, or the call was refused during a pause |
| `errors` | other failed calls |

Each node adds its counts to the day's row every flush interval: in
the `gwag-usage` bucket in a cluster (compare and set, so a row is the
cluster total), in memory standalone. Rows are kept for
`UsageOptions.Retention` (default 35 days); in a cluster this is the
bucket's max age, set by the node that creates the bucket. A
standalone gateway loses its rows on restart.

```
gwag usage                                   # last 7 days
gwag usage --from 2026-11-01 --to 2026-11-30 --service vendor --csv > november.csv
gwag usage --caller billing
```

Admin API (dev role): `GET /admin/usage?from=YYYY-MM-DD&to=YYYY-MM-DD&caller=&service=ns[:vN]`
answers `{from, to, rows}`; `&format=csv` answers `text/csv` with
the columns `day,caller,namespace,version,operation,calls,units,rate_limited,quota_exceeded,upstream_429,errors`.
The node answering flushes its own counts first; other nodes' counts
are up to one flush interval old.

The caller column holds every distinct caller id. Use a caller-id
mode that callers cannot forge (HMAC, delegated, mTLS via proxy);
in public mode a client can add rows by sending made-up ids.

## Metrics

Default Prometheus sink only, without the caller label:

| Metric | Labels | Meaning |
|---|---|---|
| `upstream_requests_total` | `namespace`, `version`, `method`, `code` | Attempts sent upstream by the upstream's answer: HTTP status (`"200"`, `"429"`), gRPC code name, `none` without a response. Calls the gateway refused are not counted; retries are. |
| `upstream_ttfb_seconds` | `namespace`, `version`, `method` | Time to the upstream's first response byte. |
| `upstream_request_bytes`, `upstream_response_bytes` | `namespace`, `version`, `method` | Body sizes. |
| `limit_rejections_total` | `namespace`, `version`, `reason` | `rate_limit`, `quota`, `paused`. |
| `upstream_pauses_total` | `namespace`, `version`, `origin` | `local` (this node got the 429), `peer`. |
| `rate_limit_wait_seconds` | `namespace`, `version` | Time calls waited for a token. |
| `rate_limit_split_peers` | none | N in the split. |

Details in [`operations.md`](./operations.md#upstream-sla).

## Cutover and freeze

Operator limits, quotas and costs are in the cluster state document
(`gwag cluster export` / `import`, [`cutover.md`](./cutover.md#state-copy)).
Quota counters and usage rows are not. A cutover freeze refuses
limit, quota and cost writes like the other admin writes.

## Not covered

- Leased blocks (see above).
- Limits declared by a service at registration (`ServiceBinding`).
  The vendor's limit is an operator fact, so it is set in code by the
  embedding gateway or with `gwag limit set`.
- Subscriptions are not rate limited, counted or paused.
- MCP upstreams have no SLA metrics (their calls go through the MCP
  client library).
