# Typed event channels

Annotate a proto message with a channel name and subject pattern.
`gwag gen` writes typed publish and listen calls next to the
language's normal protobuf output, for Go, TypeScript and Python. The
calls run over a bus: NATS JetStream (the gateway's cluster bus) by
default, an in-process queue for tests, Kafka, or your own backend.

This page covers the annotation, the generator, the calls, JetStream
and delivery. It needs no Kafka. Kafka and Confluent Schema Registry
are in [`events-kafka.md`](./events-kafka.md); per-language details,
writing a backend, checks and adding a language are in
[`events-reference.md`](./events-reference.md).

What the gateway does with the same annotations (registry, `ps_sub`
and `ps.pub`, streams, dependency edges) is in
[`pubsub.md`](./pubsub.md#named-channels). The gateway is not on the
path of a generated `publish` or `on` call.

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
`gwag/ps/v1/options.proto`) itself; no file on disk. Prefer the long
path: it matches the Go registry path and the gateway docs. A service
that registers this proto with the gateway still ships the options
file in its proto imports.

- **Channel key** — `<proto package>.<name>`, e.g.
  `orders.v1.OrderShipped`. A message with a binding but no `name`
  generates nothing (payload typing only).
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
| `--lang` | `go`, `ts` (alias `typescript`), `python` (alias `py`) |
| `--out` | output directory, default `.` |
| `-I` | import path, repeatable, default `.` |
| `--opt` | generator parameter, repeatable; passed to every generator (`paths=source_relative`, `module=...`, `M...`); `gwag` reads `events_import=MODULE` for TypeScript and does not pass it on |
| `--grpc` | also run the gRPC stub generator (Go) |
| `--module-dir` | project root that pins the generators (`go.mod`, `package.json`, `pyproject.toml`), default `.` |

`--registry`, `--subject`, `--bind`, `--import-dir`, `--lock` and
`--check-lock` generate from a Schema Registry subject; see
[Import](./events-kafka.md#import).

Files are named relative to an import path, as with `protoc`. A file
given by its disk path is mapped to the import path that contains it.

`gwag gen` does not look up generators on `PATH`. It runs the version
your project pins, so two machines generate the same bytes. If a
generator is missing, the error prints the command that adds it. Each
generated event file carries a header with the gwag version and the
base generator's version. An untagged gwag build stamps `(devel)`, so
regenerating on another commit does not change the header.

| | Base generator | Pin once | Output |
|---|---|---|---|
| Go | `go tool protoc-gen-go` (`protoc-gen-go-grpc` with `--grpc`), run in `--module-dir` | `go get -tool google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.12`; with `--grpc` also `go get -tool google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest` | `<file>.pb.go`, `<file>.events.go` (same package) |
| TypeScript | `node_modules/.bin/protoc-gen-es` of `--module-dir` | `pnpm add --save-dev --save-exact @bufbuild/protoc-gen-es@2.17.0 typescript` and `pnpm add --save-exact @bufbuild/protobuf@2.17.0` | `<file>_pb.ts`, `<file>_events.ts` |
| Python | `uv run --locked --project <module-dir> protoc-gen-python_betterproto2` | `uv add --dev betterproto2_compiler==0.10.1` and `uv add betterproto2==0.10.0` | betterproto2 modules, `<file>_events.py` |

Python needs `uv` on `PATH`; it is the only tool taken from there.
`--locked` fails when `uv.lock` is out of date. Commit the lockfiles
(`go.mod`, `pnpm-lock.yaml`, `uv.lock`); do not commit `node_modules`
or `.venv`.

TypeScript and Python need extra options and a runtime package (both
private, depend on them by path). Setup commands:
[`events-reference.md`](./events-reference.md#typescript-setup).

## Calls

One bus type per `.proto` file: `Bus` in Go (so all channels of one Go
package must be declared in one file), `<File>Bus` in TypeScript and
Python (`orders.proto` gives `OrdersBus`). Per channel `X`:

| | Go | TypeScript | Python |
|---|---|---|---|
| Create | `ordersv1.NewBus(backend)` | `new OrdersBus(backend)` | `OrdersBus(backend)` |
| Subject | `XSubject{OrderId: "o-1"}` | `{ orderId: "o-1" }` | `XSubject(order_id="o-1")` |
| Publish | `bus.PublishX(ctx, subj, msg, opts...)` | `await bus.publishX(subj, msg, opts?)` | `await bus.publish_x(subj, msg, msg_id=, headers=)` |
| Listen | `stop, err := bus.OnX(ctx, group, fn, opts...)` | `const stop = await bus.onX(group, fn, opts?)` | `stop = await bus.on_x(group, fn, options=)` |
| Union handlers | `XHandlers{OrderCreated: fn, Unknown: fn}` | `{ OrderCreated: fn, unknown: fn }` | `order_created=fn, unknown=fn` |
| Oneof handlers | `XHandlers{Captured: fn}` | `{ captured: fn }` | `captured=fn` |
| Oneof, whole message | `bus.OnXMessage` | `bus.onXMessage` | `bus.on_x_message` |
| Subscriptions | `bus.Subscriptions()` | `bus.subscriptions()` | `bus.subscriptions()` |
| Runtime access | `bus.Core()` | `bus.core` | `bus.core` |
| Stop | `stop()` | `await stop()` | `await stop()` |
| Ack / redeliver | return `nil` / `error` | resolve / throw | return / raise |
| Raw message | `events.MessageFrom(ctx)` | third handler argument | `message_from()` |

A nil or missing handler acks that type or case without handling it. A
type with no handler and no `Unknown` is acked and logged once per
(channel, type). `Unknown` gets a union type this code does not know,
or a oneof message with no case set; in a union it receives the raw
`Message`, in a oneof the whole message.

Plain and union channels of `examples/events/proto/orders/v1/orders.proto`:

```go
bus := ordersv1.NewBus(backend)

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
```

```ts
import { OrdersBus } from "./gen/orders/v1/orders_events.ts";

const bus = new OrdersBus(backend);

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
```

```python
from proto_gen.orders.v1 import OrderCreated, OrderShipped
from proto_gen.orders.v1.orders_events import OrderEventsSubject, OrdersBus, OrderShippedSubject

bus = OrdersBus(backend)

await bus.publish_order_shipped(OrderShippedSubject(order_id="o-1"), OrderShipped(carrier="ups"))
stop = await bus.on_order_shipped("notify", handler)   # async def handler(subject, message) -> None

# Union: one consumer per (channel, group), dispatch by Gwag-Type
await bus.publish_order_events(OrderEventsSubject(order_id="o-1"), OrderCreated(customer="ann"))
stop = await bus.on_order_events(
    "billing",
    order_created=on_created,      # async def (OrderEventsSubject, OrderCreated) -> None
    order_cancelled=on_cancelled,
    unknown=on_unknown,            # async def (OrderEventsSubject, Message) -> None
)
```

Oneof channels, the Go `XEvent` interface, TypeScript `XEvent` types,
publish and listen options, and logger settings per language:
[`events-reference.md`](./events-reference.md#calls-per-language).

## JetStream

The default backend. `nc` is a `*nats.Conn` (Go), a
`@nats-io/transport-node` connection (TypeScript) or a `nats-py`
client (Python).

| | Go | TypeScript | Python |
|---|---|---|---|
| Create | `jetstream.New(nc, jetstream.WithPrefetch(n))` | `new JetStreamBackend(nc, { prefetch?, logf? })` | `JetStreamBackend(nc, prefetch=16, logf=)` |
| Import | `events/jetstream` | `@gwag/events/jetstream` | `gwag_events` |

- One stream per channel named `EV_<package with . as _>_<name>`
  (`EV_orders_v1_OrderEvents`), subject = the pattern with `{x}` as `*`.
- The backend creates a missing stream (file storage, limits
  retention, MaxAge 7d, Duplicates 2m, 1 replica) and never updates an
  existing one; the gateway ensures streams with its cluster policy
  and wins.
- Republishing a message id inside the 2 minute window stores it once.
- Messages are pulled in batches (`prefetch`, default 16); lower it to
  1 to spread a group's work evenly, as prefetched messages count
  against AckWait.
- A group is a durable consumer, so messages published while no
  subscriber runs wait for the group.

Runnable, same output in all three (embedded NATS, or `NATS_URL`; a
gateway cluster's NATS client port is 14222 under `gwag up`):

```bash
go run ./examples/events/go-jetstream
cd examples/events/ts && pnpm start
cd examples/events/py && uv run python main.py
```

The in-process **memory** backend has the same group and retry
semantics and keeps dead letters for assertions. For tests:
`memory.New()` / `DeadLetters()` (Go), `new MemoryBackend()` /
`deadLetters()` from `@gwag/events/memory` (TypeScript),
`MemoryBackend()` / `dead_letters()` (Python).

## Delivery

- **Group** — the `group` argument of `On`: letters, digits, `_` and
  `-`. Every group gets every message. Subscribers sharing a group
  share its messages: run the same service three times and each
  message reaches one of the three. This holds across languages.
- **New group** — starts at new messages. `DeliverAll` starts it at
  the first stored message. Only the first subscriber's options create
  the group; later subscribers use it as it is.
- **Ack** — the handler succeeds.
- **Redelivery** — the handler fails; the message returns after the
  backoff (default 1s, 5s, 30s; the last value repeats).
- **Dead letter** — after `MaxDeliver` failed deliveries (default 5)
  the message is published to `gwag.dlq.<stream>` with the original
  headers plus `Gwag-Error`, then dropped from the channel. Nothing
  consumes the DLQ subject unless you add a stream or listener for
  `gwag.dlq.>`.
- **AckWait** — default 30s. A handler still running after it gets the
  message redelivered elsewhere. On JetStream, a delivery that times
  out on its last attempt is dropped without a dead letter.

| | Go | TypeScript | Python |
|---|---|---|---|
| Max deliveries | `events.MaxDeliver(n)` | `maxDeliver` | `max_deliver` |
| Start at first message | `events.DeliverAll()` | `deliverAll` | `deliver_all` |
| Backoff | `events.Backoff(d...)` | `backoffMs` | `backoff` |
| Ack wait | `events.AckWait(d)` | `ackWaitMs` | `ack_wait` |
| Unit | `time.Duration` | milliseconds | seconds |

## Headers and body

| Header | Value |
|---|---|
| `Gwag-Type` | full name of the body message, always set |
| `Gwag-Channel` | channel key |
| `Nats-Msg-Id` | message id on NATS (JetStream dedup) |
| `Gwag-Msg-Id` | message id on other backends |
| `Gwag-Error` | failure text, on dead letters only |

Body: proto binary. The format is the same in all three languages, so
a Go publisher can feed a Python listener and the reverse. A Serde
changes the body, not these headers; Kafka adds more
([`events-kafka.md`](./events-kafka.md#wire-format)).

## Reporting subscriptions to the gateway

The bus records each `(channel, group)` an `On` call used. Pass them
to the control plane so the gateway counts them as dependency edges
(see [`services.md`](./services.md#start-gate-subscribes)):

```go
var subs []controlclient.Subscription
for _, s := range bus.Subscriptions() {
    subs = append(subs, controlclient.Subscription{Channel: s.Channel, Group: s.Group})
}
// controlclient.Service{..., Subscribes: subs}
```

Call the `On` methods before `SelfRegister`. `events` does not import
`controlclient`, so you write this loop. TypeScript and Python have no
control-plane client; their `subscriptions()` return the same pairs.

## Security

The bus is open by default. Any client that reaches the NATS or Kafka
port can publish or subscribe on any subject or topic, including these
channels and the DLQ. Message authorship is an application concern:
sign payloads yourself if consumers must trust who sent them. gwag's
HMAC channel auth covers only `ps.pub` and `ps_sub` through the
gateway ([`pubsub.md`](./pubsub.md#auth-tiers)), not calls made with
generated code or a raw NATS or Kafka client.
