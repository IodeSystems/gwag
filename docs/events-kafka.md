# Kafka and Schema Registry

Typed events over Kafka, with or without a Confluent Schema Registry,
in Go, TypeScript and Python. Read [`events.md`](./events.md) first.
If you use JetStream, you do not need this page.

Two separate choices:

- **Backend** — where messages go. JetStream (default) or Kafka
  ([Kafka backend](#kafka-backend)).
- **Wire format** — what the body is. Plain proto (default) or
  Confluent Schema Registry framing
  ([Schema Registry](#schema-registry-options)).

Kafka with plain proto needs no registry: go to
[Kafka backend](#kafka-backend). If the topic has a schema in a
registry, start here.

## Who produces the topic?

One question decides how gwag uses a registry schema.

| | **own** | **import** | **shadow** |
|---|---|---|---|
| Who produces the topic | a gwag service | another team | another team |
| Use when | default | default | advanced: you need your own annotations on their message |
| The proto gwag compiles | yours, with `(gwag.ps.v1.binding)`; the serde registers it | read-only files fetched from their subject, pinned in `gwag.lock.json` | yours, with `(gwag.ps.v1.binding)` and `(gwag.ps.v1.shadow)` naming their subject |
| You run | `gwag schema push` (CI) | `gwag gen --registry ...` | `gwag schema check` (CI) |
| Fails at gen time | nothing (`gwag gen` does not contact the registry) | `gwag gen --registry`: subject or a reference missing, or not PROTOBUF | nothing |
| Fails in CI | `gwag schema push --dry-run`: schema not registered; `push`: registry rejects an incompatible change | `gwag gen --check-lock`: a file was edited, or the registry's version has another id or text, or is gone | `gwag schema check`: your message no longer matches the subject |
| Fails at service registration | nothing | nothing | nothing |
| Fails at runtime | publish: the subject does not hold the schema (lookup-only), or the registry rejects it | a record cannot be decoded | a record cannot be decoded; a registry change since the last check is not seen |
| Example | `srinterop produce` | `corp import` | `corp shadow` |

A record the subscriber cannot decode (unknown schema id, registry
down) fails its delivery: it is retried with the backoff, then
dead-lettered. The gateway does not read Kafka or the registry, so
nothing is checked when a service registers.

Records from another producer carry no `Gwag-*` headers. The serde
takes the message type from the schema id and message index, so typed
handlers and union dispatch work on them.

Examples, one per row, in the orders domain
(`examples/events/proto/orders/v1/orders.proto`; `corp.orders.v1.Order`
for import and shadow). Each script starts Kafka and
`cp-schema-registry` in docker and checks the rows against
Confluent's own serializers:

| | Go | TypeScript | Python |
|---|---|---|---|
| own | [`go-kafka/srinterop`](../examples/events/go-kafka/srinterop) | [`ts-kafka/src/srinterop.ts`](../examples/events/ts-kafka/src/srinterop.ts) | [`py-kafka/srinterop.py`](../examples/events/py-kafka/srinterop.py) |
| import | [`go-kafka/corp`](../examples/events/go-kafka/corp) `import` | [`ts-kafka/corp/main.ts`](../examples/events/ts-kafka/corp/main.ts) `import` | [`py-kafka/corp.py`](../examples/events/py-kafka/corp.py) `import` |
| shadow | `go-kafka/corp` `shadow` | `ts-kafka/corp/main.ts` `shadow` | `py-kafka/corp.py` `shadow` |
| script | `go-kafka/test.sh` | `ts-kafka/sr-test.sh` | `py-kafka/test.sh` |

### own

A gwag service produces the topic. The serde registers your proto.

```go
import (
    "github.com/iodesystems/gwag/v2/events"
    "github.com/iodesystems/gwag/v2/events/kafka"
    sr "github.com/iodesystems/gwag/v2/events/schemaregistry"
)

backend, _ := kafka.New(brokers)
registry, _ := sr.NewClient(sr.Config{URL: "http://localhost:8081"})
bus := ordersv1.NewBus(backend, events.WithSerde(sr.NewSerde(registry, sr.AutoRegister())))
err := bus.PublishOrderEvents(ctx, ordersv1.OrderEventsSubject{OrderId: "o-1001"},
    &ordersv1.OrderCreated{OrderId: "o-1001", Customer: "ann"})
```

```ts
import { KafkaBackend } from "@gwag/events-kafka";
import { RegistryClient, SchemaRegistrySerde } from "@gwag/events-schemaregistry";

const backend = new KafkaBackend(brokers);
const registry = new RegistryClient({ url: "http://localhost:8081" });
const bus = new OrdersBus(backend, { serde: new SchemaRegistrySerde(registry, { autoRegister: true }) });
await bus.publishOrderEvents({ orderId: "o-1001" }, { type: "orders.v1.OrderCreated", value: { orderId: "o-1001", customer: "ann" } });
```

```python
from gwag_events_kafka import KafkaBackend
from gwag_events_schemaregistry import Client, Config, Serde

backend = KafkaBackend("localhost:9092")
registry = Client(Config(url="http://localhost:8081"))
bus = OrdersBus(backend, serde=Serde(registry, auto_register=True))
await bus.publish_order_events(OrderEventsSubject(order_id="o-1001"), OrderCreated(order_id="o-1001", customer="ann"))
```

- The first publish of a file registers it under `<topic>-value`, and
  each import (here `gw/proto/ps/v1/options.proto`) under its import
  path as a reference. Later publishes use the cached id.
- Without `AutoRegister` (`autoRegister` in TypeScript,
  `auto_register` in Python) the serde only looks the schema up and
  the publish fails when the subject does not hold it. Go, TypeScript
  and Python register the same bytes, so a schema one registered is
  found by the others. Register from CI instead:

  ```bash
  gwag schema push --registry "$SR_URL" -I proto orders/v1/orders.proto
  gwag schema push --registry "$SR_URL" --dry-run -I proto orders/v1/orders.proto   # fails if not registered
  ```

  `push` registers the file of every named channel. Flags:
  `--strategy` (`topic`, `record`, `topic-record`; default `topic`),
  `--topic orders.v1.OrderEvents=orders.lifecycle` (the topic of a
  channel; default the stream name `EV_orders_v1_OrderEvents`, as the
  mapper does at runtime), `--normalize`, `-I`.
- The registry applies its compatibility level when a new version is
  registered: a publish or push of an incompatible change fails there.

### import

Another team produces the topic and gwag reads it. `gwag gen` fetches
their subject and compiles it:

```bash
gwag gen --registry "$SR_URL" --subject corp-orders-value --import-dir imported \
  --lang go --out gen --opt paths=source_relative \
  --opt 'Mregistry/corp-orders-value.proto=example.com/shop/gen/corpordersv1;corpordersv1' \
  --opt 'Mcorp/common/v1/money.proto=example.com/shop/gen/corpcommonv1;corpcommonv1'
```

- Writes `imported/registry/corp-orders-value.proto` and each
  reference under its import path (`imported/corp/common/v1/money.proto`),
  read-only (mode 0444), with a header naming the registry, subject,
  version and id. Commit them. `--import-dir` defaults to `imported`.
- Writes `imported/gwag.lock.json` (`--lock FILE` moves it): subject,
  version, schema id and sha256 of each file. `SUBJECT@VERSION` pins a
  version; the default, `latest`, is pinned to its number in the lock.
- Registry protos have no `(gwag.ps.v1.binding)`. The channel is the
  first message of the subject (`corp.orders.v1.Order`), named after
  it (`Order`), with the topic of `<topic>-value` as a literal pattern
  (`corp-orders`; dots split tokens, characters a subject token cannot
  hold become `_`). Change any of the three with
  `--bind corp-orders-value=corp.orders.v1.Order,name=Orders,pattern=corp-orders`;
  a subject that is not `<topic>-value` needs `pattern`.
- Registry protos rarely have `go_package`; give one with `--opt M...`.
- TypeScript: the same command with `--lang ts` and the options from
  [`events-reference.md`](./events-reference.md#typescript-setup);
  the imported files each get a `_pb.ts`, and the channel an
  `_events.ts` (`examples/events/ts-kafka/corp/gen.sh`).
  Python: the same command with `--lang python`
  (`examples/events/py-kafka/corp-gen.sh`).
- `gwag gen --lang go ... --import-dir imported` regenerates from the
  committed files without the registry. `gwag gen --registry URL
  --import-dir imported` without `--subject` fetches the locked
  versions again.

```go
backend, _ := kafka.New(brokers, kafka.WithTopics(kafka.TopicTable(map[string]kafka.Topic{
    "corp.orders.v1.Order": {Name: "corp-orders"},
})))
bus := corpordersv1.NewBus(backend, events.WithSerde(sr.NewSerde(registry)))
stop, err := bus.OnOrder(ctx, "shop", func(ctx context.Context, _ corpordersv1.OrderSubject, m *corpordersv1.Order) error {
    log.Printf("%s %s", events.MessageFrom(ctx).Key, m.GetOrderId()) // record key, e.g. o-3001
    return nil
})
```

```ts
const backend = new KafkaBackend(brokers, { topics: topicTable({ "corp.orders.v1.Order": { name: "corp-orders" } }) });
const bus = new CorpOrdersValueBus(backend, { serde: new SchemaRegistrySerde(registry) });
await bus.onOrder("shop", (_subject, m, raw) => {
  console.log(Buffer.from(raw.key!).toString(), m.orderId); // record key, e.g. o-3001
});
```

```python
from gen_corp.corp.orders.v1 import Order
from gen_corp.corp.orders.v1.corp_orders_value_events import CorpOrdersValueBus, OrderSubject

backend = KafkaBackend(brokers, topics=topic_table({"corp.orders.v1.Order": Topic(name="corp-orders")}))
bus = CorpOrdersValueBus(backend, serde=Serde(registry))

async def on_order(s: OrderSubject, m: Order) -> None:
    print(message_from().key, m.order_id)   # record key, e.g. b"o-3001"

await bus.on_order("shop", on_order)
```

The pattern has no wildcards, so a record without `Gwag-*` headers gets
the pattern as its subject; the record key is in `Message.Key`
(`Message.key` in TypeScript and Python).

In CI:

```bash
gwag gen --check-lock --import-dir imported
```

Fails when an imported file was edited, or when the registry's subject
version has another id or another text, or is gone. A newer version
upstream is a note, not a failure: the lock pins on purpose. To move,
re-import with `--subject corp-orders-value@latest` and review the
diff.

### shadow

Advanced. Another team produces the topic, and you need gwag
annotations on their message: fewer fields, your own package, your own
channel binding. If you only need to read their topic, use
[import](#import). `gwag schema check` compares the shadow with the
registry in CI. A gateway whose own registry holds the imported
subject also checks it when the service registers, as a warning
([below](#check-at-register)). A registry change after either run is
not seen until the next one.

```proto
message CorpOrder {
  option (gwag.ps.v1.binding) = { name: "CorpOrders", pattern: "corp-orders" };
  option (gwag.ps.v1.shadow) = {
    registry: "corp"                  // a name; --registry corp=URL gives the URL
    subject: "corp-orders-value"
    version: "latest"                 // or "3"
    message: "corp.orders.v1.Order"   // default: same simple name, else the first message
  };
  string order_id = 1;
  Money total = 2;
}
```

```bash
gwag schema check --registry corp="$SR_URL" -I proto shop/v1/order.proto
```

- `side: CONSUMER` (default) — gwag reads the topic. The shadow
  message may leave fields out. Each field it keeps must match the
  registry field of the same number: type, cardinality (singular,
  repeated, map) and enum values. A field number the registry does not
  have fails. Nested messages are compared the same way. A different
  field name is a warning (the wire carries numbers).
- `side: PRODUCER` — gwag writes the topic. gwag swaps the shadow
  message into the registry's schema and asks the registry's
  compatibility API
  (`POST /compatibility/subjects/{subject}/versions/{version}`), so the
  subject's own level (`BACKWARD`, `FULL`, ...) decides.
- Exit 1 with one line per problem:

  ```
  FAIL drift.v1.Order (consumer) vs corp corp-orders-value@latest
    error   total: field 2 is string, registry has message corp.common.v1.Money
    error   coupon: field 9 is not in registry message corp.orders.v1.Order
  ```

Records are framed as the registry's type. Map it to the shadow
message so typed handlers get them:

```go
serde := sr.NewSerde(registry, sr.TypeNames(map[string]string{"corp.orders.v1.Order": "shop.v1.CorpOrder"}))
```

```ts
const serde = new SchemaRegistrySerde(registry, { typeNames: { "corp.orders.v1.Order": "shop.v1.CorpOrder" } });
```

```python
serde = Serde(registry, type_names={"corp.orders.v1.Order": "shop.v1.CorpOrder"})
```

The shadow proto is in `go-kafka/corp/shadow/shop/v1/order.proto`;
`go-kafka/corp/shadow/drift/v1/drift.proto` is one that fails the
check. TypeScript uses the Go example's shadow proto; `gwag schema
check` is the same command for every language.

#### Check at register

With the gateway's Schema Registry on (`--schema-registry`) and the
corp registry imported into a context (`gwag schema import --into
.corp`), the gateway checks each shadow message of a service when the
service registers through the control plane. The comparison is the
one `gwag schema check` runs (`side` decides consumer or producer).

- **Warn, never reject.** The registration succeeds. A difference
  goes to the gateway log, to the metric
  `shadow_check_total{outcome="mismatch"}`, to
  `RegisterResponse.warnings` (log them in the client), and to
  `ServiceInfo.warnings` (`GET /admin/services` `warnings`,
  `gwag services list`).
- **Lookup.** `registry` in the option is the context name: `corp` is
  `.corp`. If that context holds `subject`, the gateway checks there.
  If not, it tries the other non-default contexts in name order and
  uses the first that holds the subject. The default context holds
  gwag's own channels and is never searched. `version` and `message`
  work as in `gwag schema check`.
- **No check, no warning** when the registry is off, or no context
  holds the subject (metric `outcome="not_found"`). The gateway reads
  only its own registry; it never calls the corp registry.
- **When.** At Register. A later import of a new subject version is
  not seen until the service registers again. `ServiceInfo.warnings`
  is the answer of the latest Register this gateway took; other
  nodes of a cluster do not have it.

## One id space per producer

A record carries a schema id, and the id means something only in the
registry that gave it out. Each topic has one producing system, so
each topic belongs to one id space: the producer's.

| | **own** | **import** | **shadow** |
|---|---|---|---|
| Producer | a gwag service | another team | another team |
| Id space | gwag's: the default context of gwag's registry (`/schema-registry`), or your registry | theirs: their registry, or the context it was imported into (`/schema-registry/contexts/.corp`) | theirs, as import |
| Registry URL of the serde | gwag's | theirs | theirs |

While the other team runs its own registry, there is nothing to do:
each serde points at the registry of its topics. When gwag replaces
their registry ([schema-registry.md](./schema-registry.md)),
`gwag schema import --into .corp` copies it into the context `.corp`,
ids unchanged, and the clients of their topics move to
`http://HOST:8080/schema-registry/contexts/.corp`. gwag's own channels
stay in the default context. The two id spaces never clash: id 5 of
`.corp` and id 5 of `.` can be different schemas, and each record
decodes through its own URL.

One serde for both kinds of channel takes the other registry per
channel:

```go
own, _ := sr.NewClient(sr.Config{URL: "http://gwag:8080/schema-registry"})
corp, _ := sr.NewClient(sr.Config{URL: "http://gwag:8080/schema-registry/contexts/.corp"})
serde, err := sr.NewSerde(own, sr.AutoRegister(), sr.ChannelClient("corp.orders.v1.Order", corp))
```

```ts
const serde = new SchemaRegistrySerde(own, { autoRegister: true, channelClients: { "corp.orders.v1.Order": corp } });
```

```python
serde = Serde(own, auto_register=True, channel_clients={"corp.orders.v1.Order": corp})
```

The key is the channel key: for an import, `<package>.<message name>`
(or the `--bind` name); for a shadow, the package and the binding
name. Publish and delivery on that channel use `corp`; every other
channel uses `own`. A separate bus per kind, each with its own serde,
works as well. `AutoRegister` covers every channel of the serde: a
publish to a corp channel would register in their context. For a
service that also publishes to their topics, use a separate serde
without it.

## Kafka backend

`github.com/iodesystems/gwag/v2/events/kafka`, its own Go module (the
root module has no Kafka dependency):

```bash
go get github.com/iodesystems/gwag/v2/events/kafka
```

```go
backend, err := kafka.New([]string{"localhost:9092"}, opts...)
bus := ordersv1.NewBus(backend)
```

```ts
import { KafkaBackend } from "@gwag/events-kafka";

const backend = new KafkaBackend(["localhost:9092"], opts);
const bus = new OrdersBus(backend);
```

```python
from gwag_events_kafka import KafkaBackend

backend = KafkaBackend("localhost:9092", **opts)   # or a list of brokers
bus = OrdersBus(backend)
```

Defaults: topic per channel named by its stream
(`EV_orders_v1_OrderEvents`); record key = subject; consumer group
`<topic>.<group>`; retries in process (the partition waits); DLQ topic
`gwag.dlq.<topic>`. No dedup: Kafka does not drop a repeated
`Gwag-Msg-Id`, so handlers must be idempotent. The producer is
idempotent with `acks=all`. A group spreads over at most as many
subscribers as the topic has partitions.

Runnable: [`go-kafka`](../examples/events/go-kafka),
[`ts-kafka`](../examples/events/ts-kafka),
[`py-kafka`](../examples/events/py-kafka); each `test.sh` runs against
`apache/kafka` in docker.

### Options

| Go | TypeScript | Python | Default |
|---|---|---|---|
| `WithTopics(mapper)` | `topics: topicTable({...})` or any `(ch) => Topic` | `topics=mapper` | topic per channel, named by its stream |
| `WithPartitions(n)`, `WithReplication(n)` | `partitions`, `replication` | `partitions=n`, `replication=n` | 3, 1, for topics the backend creates; replication -1 is the broker default |
| `WithClientOpts(kgo.Opt...)` | `client`: kafkajs options for every client (`ssl`, `sasl`, `clientId`, timeouts, `retry`) | `client_config={...}`: aiokafka keyword arguments for every client (`security_protocol`, `ssl_context`, `sasl_mechanism`, `sasl_plain_username`, ...) | SASL, TLS, client id, timeouts |
| `WithProducer(*kgo.Client)` | none | `producer_config={...}`, `consumer_config={...}` (settings for the producer or the consumers only) | Go: publish through your client (`Close` leaves it open) |
| none | none | `logf=` | logger |

Notes per language:

- **Go** — franz-go; the producer uses its defaults (idempotent,
  `acks=all`).
- **TypeScript** — kafkajs's idempotent producer with `acks: -1` and
  one request in flight, so a retried batch cannot overtake a later
  one. `@gwag/events-kafka` is private, like `@gwag/events`: depend on
  it by path (`"@gwag/events-kafka": "link:../gwag/ts/events-kafka"`).
  It brings `kafkajs` and `snappyjs`; `@gwag/events` has no Kafka
  dependency. kafkajs reads gzip only, and franz-go compresses with
  Snappy by default, so the package registers a Snappy codec to read
  what a Go service writes. LZ4 and ZSTD need your own codec in
  kafkajs's `CompressionCodecs`.
- **Python** — `py/gwag-events-kafka`, aiokafka, not published; add it
  by path like `gwag-events`. The producer is idempotent with
  `acks="all"`. aiokafka reads snappy, lz4 and zstd batches (the
  package depends on `aiokafka[snappy,lz4,zstd]`): franz-go compresses
  with snappy by default, so a Python reader of a Go-written topic
  needs them.

SASL/SCRAM over TLS:

```go
backend, err := kafka.New(brokers, kafka.WithClientOpts(
    kgo.DialTLSConfig(&tls.Config{MinVersion: tls.VersionTLS12}),
    kgo.SASL(scram.Auth{User: user, Pass: pass}.AsSha512Mechanism()), // github.com/twmb/franz-go/pkg/sasl/scram
))
```

```ts
const backend = new KafkaBackend(brokers, {
  client: { ssl: true, sasl: { mechanism: "scram-sha-512", username: user, password: pass } },
});
```

### Topic mapping

A mapper returns a `kafka.Topic` (`Topic` in TypeScript and Python)
per channel; zero fields keep the default. `kafka.TopicTable`
(`topicTable`, `topic_table`) maps channel keys:

```go
kafka.WithTopics(kafka.TopicTable(map[string]kafka.Topic{
    "orders.v1.OrderEvents": {
        Name:        "orders.lifecycle.v1",                                         // default EV_orders_v1_OrderEvents
        Partitions:  12,
        Replication: 3,
        Key:         func(subject string, m *events.Message) []byte { ... },        // default: the subject
        Subject:     func(r *kgo.Record) string { ... },                            // records from other producers
        Group:       func(g string) string { return "shop-" + g },                  // default <topic>.<group>
        DLQ:         "orders.lifecycle.v1.dlq",                                     // default gwag.dlq.<topic>
        Retry:       func(g string) string { return "orders.lifecycle.v1.retry." + g }, // default gwag.retry.<topic>.<group>
    },
}))
```

```ts
topics: topicTable({
  "orders.v1.OrderEvents": {
    name: "orders.lifecycle.v1",                         // default EV_orders_v1_OrderEvents
    partitions: 12,
    replication: 3,
    key: (subject, m) => subject,                        // default: the subject
    subject: (r) => String(r.key),                       // records from other producers
    group: (g) => `shop-${g}`,                           // default <topic>.<group>
    dlq: "orders.lifecycle.v1.dlq",                      // default gwag.dlq.<topic>
    retry: (g) => `orders.lifecycle.v1.retry.${g}`,      // default gwag.retry.<topic>.<group>
  },
}),
```

Python: `Topic(name=, partitions=, replication=, key=, subject=, group=,
dlq=, retry=)` with the same defaults, and
`topic_table({"orders.v1.OrderEvents": Topic(...)})`. `subject` takes
the aiokafka `ConsumerRecord`; `key` takes `(subject, message)` and
returns bytes.

- When `Key` returns something other than the subject, the subject
  rides in `Gwag-Subject`.
- A record without `Gwag-Subject` and `Gwag-Channel` came from another
  producer. Its subject is `Subject(record)`; the default is the
  pattern when it has no wildcards, else the record key. A subject
  that does not match the pattern is skipped (acked, logged once).
- The backend creates the DLQ and retry topics at subscribe.

### Retries

Chosen per subscribe:

- `kafka.RetryInProcess()` (`retryInProcess()`, `retry_in_process(opts)`;
  default) — the subscriber retries with the backoff; later records on
  that partition wait.
- `kafka.RetryTopic()` (`retryTopic()`, `retry_topic(opts)`) — a failed
  record goes to the group's retry topic with `Gwag-Attempt` and
  `Gwag-Retry-At` (Unix milliseconds), and the partition moves on. A
  second consumer delivers it again when due; after `MaxDeliver`
  failed deliveries it goes to the DLQ. Order per key is lost across a
  retry, and a record due later holds up the retry records behind it.

```go
stop, err := bus.OnOrderEvents(ctx, "billing", handlers, kafka.RetryTopic(), events.MaxDeliver(5))
```

```ts
const stop = await bus.onOrderEvents("billing", handlers, { ...retryTopic(), maxDeliver: 5 });
```

```python
from gwag_events_kafka import retry_topic

stop = await bus.on_order_events("billing", order_created=on_created,
                                 options=retry_topic(SubscribeOptions(max_deliver=5)))
```

## Schema Registry options

Go: `sr.NewClient(sr.Config{...})` and `sr.NewSerde(client, opts...)`
from `events/schemaregistry`; pass the serde with
`events.WithSerde(serde)`. TypeScript: `new RegistryClient({...})` and
`new SchemaRegistrySerde(client, {...})`, passed as
`new OrdersBus(backend, { serde })`. Python: `Client(Config(...))` and
`Serde(client, ...)`, passed as `OrdersBus(backend, serde=serde)`. The
TypeScript serde needs the generated message schema, so publish
through a generated bus; `core.publish` with only a type name fails.

| Go | TypeScript | Python | Meaning |
|---|---|---|---|
| `sr.Config{URL, Username, Password, Token, TLS, HTTPClient}` | `{ url, username, password, token, fetch }` | `Config(url, username, password, token, verify, http_client)` (httpx) | basic auth (also from URL user info), bearer token (ignored when a username is set). Go: TLS config. TypeScript: your own `fetch` (an undici `dispatcher` for a custom CA). Python: `verify` is `False`, a CA bundle path or an `ssl.SSLContext` |
| `WithSubjectNameStrategy(s)` | `subjectNameStrategy` | `subject_name_strategy=` | `TopicNameStrategy` (default, `<topic>-value`), `RecordNameStrategy` (message full name), `TopicRecordNameStrategy` (`<topic>-<message full name>`). TypeScript: `topicNameStrategy`, `recordNameStrategy`, `topicRecordNameStrategy`, `strategyByName`. Python: `topic_name_strategy`, `record_name_strategy`, `topic_record_name_strategy`, `strategy_by_name("topic")`. Go: `StrategyByName` |
| `AutoRegister()` | `autoRegister: true` | `auto_register=True` | register on first publish (own) |
| default | default | default | look up only; publish fails when not registered |
| `UseLatest()` | `useLatest: true` | `use_latest=True` | frame with the subject's latest schema (Confluent's `use.latest.version`) |
| `UseSchemaID(id)` | `useSchemaId: id` | `use_schema_id=id` | frame with a fixed id |
| `Normalize()` | `normalize: true` | `normalize=True` | `normalize=true` on lookups and registrations |
| `WithReferenceSubject(f)` | `referenceSubject` | `reference_subject=f` | subject of an import; default the import path |
| `TypeNames(map)` | `typeNames` | `type_names={...}` | registry type name to local type name (shadow); decode only |
| `ChannelClient(key, c)` (repeatable) | `channelClients: { key: c }` | `channel_clients={key: c}` | registry of one channel, on publish and on delivery ([one id space per producer](#one-id-space-per-producer)) |

`AutoRegister`, `UseLatest` and `UseSchemaID` are alternatives. Set at
most one. A second one fails at construction with
`schema registry: auto-register, use-latest and use-schema-id are mutually exclusive; set at most one`:
Go `NewSerde` returns `sr.ErrExclusiveModes`, TypeScript throws `Error`,
Python raises `ValueError`. An empty channel key or a missing client
in a channel client fails the same way with
`schema registry: channel client for channel "KEY": want a channel key and a client`.

On decode, the message type comes from the schema id: the registry's
`format=serialized` (base64 `FileDescriptorProto`), or, in TypeScript,
the `.proto` text when the registry returns text. Python reads
`format=serialized` only: the message tree is all it needs.
`gwag_events_schemaregistry.srtest.FakeRegistry` is an in-memory
registry for Python tests.

Schemas are registered as Confluent's own clients do: base64 of the
`FileDescriptorProto`, imports as references. `google/protobuf/*`,
`google/type/*` and `confluent/*` imports are the registry's built-ins
and get no reference; on decode only `google/protobuf/*` is known.
Fetched schemas and ids are cached per client.

The CLI (`gwag gen --registry`, `--check-lock`, `gwag schema check`,
`gwag schema push`) takes credentials from the URL user info or the
environment: `GWAG_REGISTRY_USERNAME` / `GWAG_REGISTRY_PASSWORD`
(basic), `GWAG_REGISTRY_TOKEN` (bearer), `GWAG_REGISTRY_CA` (a CA file
for HTTPS). `gwag schema check` takes `--registry NAME=URL` (repeatable);
the others take `--registry URL`.

## Wire format

`schemaregistry.Serde` (`SchemaRegistrySerde` in TypeScript, `Serde` in
Python) writes Confluent's protobuf framing as the body:

| Bytes | Value |
|---|---|
| 1 | magic byte `0` |
| 4 | schema id, big endian |
| varint array | message indexes: path of the message in its file, zigzag varints, count first; `[0]` (the first message) is a single `0` |
| rest | proto binary |

The `Gwag-*` headers are still set. On delivery the serde strips the
framing, sets `Gwag-Type` from the schema when the record has none,
and adds `Gwag-Schema-Id` (in the handler only). A body that does not
start with `0` is plain proto and passes through, so a topic can move
to framing without a cut-over.

Kafka adds these headers:

| Header | Value |
|---|---|
| `Gwag-Subject` | subject, when the record key is not the subject |
| `Gwag-Attempt` | delivery attempt, on a retry topic |
| `Gwag-Retry-At` | when a retry record is due, Unix milliseconds |

What the checks cover: `go-kafka/test.sh` checks this framing against
Confluent's own code: the Java `kafka-protobuf-console-consumer` and
the Python `ProtobufDeserializer` read what gwag writes, and gwag reads
what the Python `ProtobufSerializer` writes. `py-kafka/test.sh` checks
the Python serde the same way: Go-written records read by Python,
Python-written records read by Confluent's `ProtobufDeserializer` and
by Go, Confluent-written records (no `Gwag-*` headers) read by Python
typed handlers, and the two languages registering one schema id.
`ts-kafka/sr-test.sh` does the same for TypeScript, adds the Java
console producer, and checks that Go and TypeScript produce the same
schema (one id, one version) and read each other's records. Commands
and ports: [`events-reference.md`](./events-reference.md#checks).
