# Typed event channels

Annotate a proto message with a channel name and subject pattern.
`gwag gen` writes typed `Publish<Channel>` / `On<Channel>` calls next
to the language's normal protobuf output. The calls run over a
pluggable bus: NATS JetStream (the gateway's cluster bus), an
in-process queue for tests, or your own backend (a Kafka one is in
[`examples/events/go-kafka/`](../examples/events/go-kafka)).

Runnable example: [`examples/events/`](../examples/events).

## Annotation

```proto
import "gw/proto/ps/v1/options.proto";

message OrderShipped {
  option (gwag.ps.v1.binding) = {
    name: "OrderShipped"
    pattern: "events.orders.{orderId}.shipped"
  };
  string order_id = 1;
  string carrier = 2;
}
```

`gwag gen` serves `gw/proto/ps/v1/options.proto` (and the short path
`gwag/ps/v1/options.proto`) itself; no copy on disk. Prefer the long
path: it matches the Go registry path and the gateway docs
([pubsub.md](./pubsub.md)). A service that registers this proto with
the gateway still ships the options file in its proto imports.

- **Channel key** — `<proto package>.<name>`, e.g.
  `orders.v1.OrderShipped`. A message with a binding but no `name`
  generates nothing (payload typing only, as before).
- **Pattern** — NATS subject grammar. `{orderId}` matches one token
  and is named; `*` matches one token and is named `P1`, `P2`, ... in
  order; a trailing `>` matches the rest and is named `Rest`. Publish
  fills the segments; listen subscribes with the wildcards and hands
  the segment values to the handler.

Three channel shapes:

- **Plain** — one message carries the name.
- **Oneof** — that one message's fields all sit in one `oneof`.
  Handlers can take the whole message or one case at a time.
- **Union** — several messages in one package share the name and the
  pattern. The `Gwag-Type` header says which one a message is.
  Same name with different patterns is a gen error.

Each channel is one JetStream stream, so two channels' patterns must
not overlap (`events.orders.*.shipped` and `events.orders.>` cannot
both be streams).

## Generating

```bash
gwag gen --lang go --out gen -I proto --opt paths=source_relative orders/v1/orders.proto
```

| Flag | Meaning |
|---|---|
| `--lang` | `go`, `ts` (Python: see below) |
| `--out` | output directory, default `.` |
| `-I` | import path, repeatable, default `.` |
| `--opt` | generator parameter, repeatable; passed to every generator (`paths=source_relative`, `module=...`, `M...`) |
| `--grpc` | also run the gRPC stub generator |
| `--module-dir` | project root that pins the generators, default `.` |

Files are named relative to an import path, as with `protoc`. A file
given by its disk path is mapped to the import path that contains it.

### Pinned generators

`gwag gen` does not look up generators on `PATH`. It runs the version
your project pins, so two machines generate the same bytes:

- **go** — `go tool protoc-gen-go` (and `go tool protoc-gen-go-grpc`
  with `--grpc`), run in `--module-dir`. Pin it once:

  ```bash
  go get -tool google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.12
  go get -tool google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest   # with --grpc
  ```

  If the tool is missing, the error prints the `go get -tool` line.
- **ts** — `node_modules/.bin/protoc-gen-es` of `--module-dir`; see
  [TypeScript](#typescript).

Generated event files carry a header with the gwag version and the
base generator's version. An untagged gwag build stamps `(devel)`, so
regenerating on another commit does not change the header.

## Go API

`<file>.events.go` lands in the same package as `<file>.pb.go` (the
proto's `go_package`). For the example proto:

```go
bus := ordersv1.NewBus(backend)

// Plain
err := bus.PublishOrderShipped(ctx, ordersv1.OrderShippedSubject{OrderId: "o-1"}, &ordersv1.OrderShipped{Carrier: "ups"})
stop, err := bus.OnOrderShipped(ctx, "notify", func(ctx context.Context, s ordersv1.OrderShippedSubject, m *ordersv1.OrderShipped) error {
    return nil // ack
})

// Union: one consumer per (channel, group), dispatch by Gwag-Type
err = bus.PublishOrderEvents(ctx, ordersv1.OrderEventsSubject{OrderId: "o-1"}, &ordersv1.OrderCreated{})
stop, err = bus.OnOrderEvents(ctx, "billing", ordersv1.OrderEventsHandlers{
    OrderCreated:   func(ctx context.Context, s ordersv1.OrderEventsSubject, m *ordersv1.OrderCreated) error { return nil },
    OrderCancelled: func(ctx context.Context, s ordersv1.OrderEventsSubject, m *ordersv1.OrderCancelled) error { return nil },
    Unknown:        func(ctx context.Context, s ordersv1.OrderEventsSubject, m *events.Message) error { return nil },
})

// Oneof: per case, or the whole message with OnPaymentsMessage
stop, err = bus.OnPayments(ctx, "ledger", ordersv1.PaymentsHandlers{
    Captured: func(ctx context.Context, s ordersv1.PaymentsSubject, m *ordersv1.PaymentCaptured) error { return nil },
})
```

Per channel `X`:

- `XSubject` — one exported `string` field per pattern segment.
- `PublishX(ctx, XSubject, msg, ...events.PublishOption)` — options
  `events.MsgID(id)` (default random) and `events.Header(k, v)`.
- `OnX(ctx, group, handler, ...events.SubscribeOption) (stop, err)`.
- Union: `XEvent`, a sealed interface the member messages implement
  (marker method in the generated file), and `XHandlers`.
- Oneof: `XHandlers` with one field per case, and `OnXMessage` for
  the whole message.
- A nil field in `XHandlers` acks that type or case without handling
  it. A type with no handler and no `Unknown` is acked and logged once
  per (channel, type).
- `events.MessageFrom(ctx)` inside a handler returns the raw message:
  subject, id, headers, delivery attempt.

All channels of one Go package must be declared in one `.proto` file:
each file gets a `Bus` type.

## Wire format

| Header | Value |
|---|---|
| `Gwag-Type` | full name of the body message, always set |
| `Gwag-Channel` | channel key |
| `Nats-Msg-Id` | message id on NATS (JetStream dedup) |
| `Gwag-Msg-Id` | message id on other backends |

Body: proto binary. Dead-lettered messages add `Gwag-Error`.

## Groups, acks, dead letters

- **Group** — `OnX(ctx, "billing", ...)`. Every group gets every
  message. Subscribers sharing a group share its messages: run the
  same service three times and each message reaches one of the three.
  On JetStream a group is a durable consumer, so messages published
  while no subscriber runs wait for the group.
- **New group** — starts at new messages. `events.DeliverAll()` starts
  it at the first stored message. Only the first subscriber's options
  create the group; later subscribers use it as it is.
- **Ack** — the handler returns nil.
- **Redelivery** — the handler returns an error. The message comes
  back after the backoff (`events.Backoff(...)`, default 1s, 5s, 30s;
  last value repeats).
- **Dead letter** — after `events.MaxDeliver(n)` failed deliveries
  (default 5) the message is published to `gwag.dlq.<stream>` with the
  original headers plus `Gwag-Error`, then dropped from the channel.
  Nothing consumes the DLQ subject unless you add a stream or
  listener for `gwag.dlq.>`.
- **AckWait** — `events.AckWait(d)`, default 30s. A handler still
  running after it gets the message redelivered elsewhere. A delivery
  that times out on its last attempt is dropped by JetStream without a
  dead letter.

## Backends

```go
type Backend interface {
    Publish(ctx context.Context, ch events.Channel, m *events.Message) error
    Subscribe(ctx context.Context, ch events.Channel, group string, opts events.SubscribeOptions, h events.Handler) (stop func(), err error)
    Close() error
}
```

The backend owns delivery: ack on nil, redeliver after
`opts.Delay(n)`, dead-letter after `opts.MaxDeliver`.
`events/eventstest.Run` is the behaviour suite every backend should
pass.

- **`events/jetstream`** — `jetstream.New(nc)`. One stream per channel
  named `EV_<package with . as _>_<name>` (`EV_orders_v1_OrderEvents`),
  subject = the pattern with `{x}` as `*`. The backend creates a
  missing stream (file storage, limits retention, MaxAge 7d,
  Duplicates 2m, 1 replica) and never updates an existing one; the
  gateway ensures streams with its cluster policy and wins. Republishing
  a message id inside the 2 minute window stores it once.
- **`events/memory`** — `memory.New()`. In-process, same group and
  retry semantics, `DeadLetters()` for assertions. For tests.
- **Kafka** — [`examples/events/go-kafka/`](../examples/events/go-kafka),
  its own Go module (franz-go). Topic per channel (stream name), record
  key = subject, consumer group `<stream>.<group>`, retries in process
  (the partition waits), DLQ topic `gwag.dlq.<stream>`. No dedup:
  Kafka does not drop a repeated `Gwag-Msg-Id`. `test.sh` runs it
  against `apache/kafka` in docker.

## Reporting subscriptions to the gateway

The bus records each `(channel, group)` an `On*` call used. Pass them
to the control plane so the gateway counts them as dependency edges:

```go
var subs []controlclient.Subscription
for _, s := range bus.Subscriptions() {
    subs = append(subs, controlclient.Subscription{Channel: s.Channel, Group: s.Group})
}
// controlclient.Service{..., Subscribes: subs}
```

Call the `On*` methods before `SelfRegister`. `events` does not import
`controlclient`, so the copy is yours.

## Security

The bus is open by default. Any client that reaches the NATS port can
publish or subscribe on any subject, including these channels and the
DLQ subjects. Message authorship is an application concern: sign
payloads yourself if consumers must trust who sent them.

## TypeScript

Setup in your project (pnpm, exact versions, lockfile committed):

```bash
pnpm add --save-dev --save-exact @bufbuild/protoc-gen-es@2.17.0 typescript
pnpm add --save-exact @bufbuild/protobuf@2.17.0
```

`@gwag/events` is the runtime. It is private for now: depend on it by
path (`"@gwag/events": "link:../gwag/ts/events"`). It ships TypeScript
source, so run it with Node 24 or newer (type stripping) or a
bundler.

```bash
gwag gen --lang ts --out src/gen -I proto \
  --opt target=ts --opt import_extension=ts --opt erasable_syntax=true \
  orders/v1/orders.proto
```

`gwag gen` runs `node_modules/.bin/protoc-gen-es` of `--module-dir`
(default `.`); it never looks on `PATH`. If it is missing, the error
prints the `pnpm add` line. It writes `orders/v1/orders_pb.ts` (the
messages) and `orders/v1/orders_events.ts` (the channels). `--lang
typescript` is the same as `--lang ts`.

Options go to protoc-gen-es as they are. Use `import_extension=ts` and
`erasable_syntax=true` to run the output directly with Node
(`erasable_syntax` turns enums into plain objects). `events_import=MODULE`
is read by gwag and not passed on: it names the module the events file
imports the runtime from, default `@gwag/events`.

The proto must import `gw/proto/ps/v1/options.proto` (the long path).
The generated `_pb.ts` imports its TypeScript from
`@gwag/events/proto/...`, so no copy of the options file is generated
next to your code. Pass your own `map_imports=...` to change that.

The same calls as Go. Messages are protobuf-es messages; publish
takes a message or its init object (`MessageInitShape`):

```ts
import { OrdersBus } from "./gen/orders/v1/orders_events.ts";

const bus = new OrdersBus(backend);

// Plain
await bus.publishOrderShipped({ orderId: "o-1" }, { carrier: "ups" });
const stop = await bus.onOrderShipped("notify", async (subject, msg, raw) => {
  // subject.orderId, msg.carrier; resolving acks, throwing redelivers
});

// Union: a tagged event to publish, a handler map (or one function) to listen
await bus.publishOrderEvents({ orderId: "o-1" }, { type: "orders.v1.OrderCreated", value: { customer: "ann" } });
await bus.onOrderEvents("billing", {
  OrderCreated: async (s, m) => {},
  OrderCancelled: async (s, m) => {},
  unknown: async (s, raw) => {},
});
await bus.onOrderEvents("audit", async (s, event) => {
  if (event.type === "orders.v1.OrderCreated") event.value.customer;
});

// Oneof: per case, or the whole message with onPaymentsMessage
await bus.onPayments("ledger", { captured: async (s, m) => {}, refunded: async (s, m) => {} });
```

Per channel `X`:

- `XSubject` — `{ orderId: string }`: one property per pattern segment,
  lower camel case (`{order_id}` is `orderId`, `*` is `p1`, `>` is
  `rest`).
- `publishX(subject, msg, opts?)` — `opts` is `{ msgId?, headers? }`.
- `onX(group, handler, opts?) => Promise<stop>` — `opts` is
  `{ maxDeliver?, deliverAll?, backoffMs?: number[], ackWaitMs? }`,
  the same knobs as Go in milliseconds. `stop()` ends this subscriber
  and does not wait for a running handler.
- Handlers get `(subject, msg, raw)`; `raw` is the `Message`: subject,
  id, headers, data, delivery attempt.
- Union: `XEvent` is `{ type: "pkg.Msg"; value: Msg } | ...`, `XEventInit`
  is the same with init objects as values, and `XHandlers` has one
  optional key per message (its protobuf-es name) plus `unknown`. A
  missing key acks that type; with no `unknown` an unknown type is
  acked and logged once.
- Oneof: `XHandlers` has one optional key per case (protobuf-es
  property name) plus `unknown`, which gets the whole message.
- `bus.subscriptions()` and `bus.core` as in Go. `new OrdersBus(backend,
  { logf })` sets the logger for skipped messages.

One difference from Go: 64-bit integers are `bigint`.

Backends (`@gwag/events/...`):

- **`jetstream`** — `new JetStreamBackend(nc, { prefetch?, logf? })`,
  `nc` from `@nats-io/transport-node`. Same stream names, durable
  consumers, stream config, ack / nak / dead-letter behaviour as the
  Go backend, so Go and TypeScript members of one group share its
  messages. `prefetch` defaults to 16; use 1 to spread work evenly.
- **`memory`** — `new MemoryBackend()`, `deadLetters()` for assertions.
- **`eventstest`** — `runBackendSuite(name, harness)`, the port of
  `events/eventstest` for `node:test`.
- **Kafka** — [`examples/events/ts-kafka/`](../examples/events/ts-kafka),
  kafkajs, the same mapping as the Go one (topic = stream name,
  record key = subject, group `<stream>.<group>`, DLQ topic
  `gwag.dlq.<stream>`).

Checks: [`examples/events/ts/`](../examples/events/ts) is the generated
example, and `examples/events/interop.sh` runs a Go publisher into a
TypeScript listener, the reverse, and a Go and a TypeScript member of
one group on one NATS (docker). `ts/events/test.sh` runs the runtime
tests against NATS in docker.

## Python

TODO (E4): `--lang python`, `protoc-gen-python_betterproto2` via
`uv run`.

## Adding a language

A language is an `eventgen.Language` registered from `init` in
`gw/eventgen/<lang>.go`:

```go
eventgen.Register(&eventgen.Language{
    Name:    "typescript",
    Plugins: func(ctx context.Context, env eventgen.Env) ([]eventgen.Plugin, error) { ... },
    Emitter: eventgen.EmitterFunc(func(in *eventgen.Input) ([]*pluginpb.CodeGeneratorResponse_File, error) { ... }),
})
```

`Plugins` resolves the pinned base generators with
`eventgen.ResolvePlugin(ctx, name, argv, env.ModuleDir, installHint)`.
`Emitter` gets the plugin request, the compiled files and
`[]*eventgen.Channel` (key, parsed pattern, kind, messages) and returns
the event files.
