# Typed event channels

Annotate a proto message with a channel name and subject pattern.
`gwag gen` writes typed publish and listen calls next to the
language's normal protobuf output, for Go, TypeScript and Python. The
calls run over a pluggable bus: NATS JetStream (the gateway's cluster
bus), an in-process queue for tests, or your own backend (Kafka
examples for all three languages are in
[`examples/events/`](../examples/events)).

What the gateway does with the same annotations (registry, `ps_sub`
and `ps.pub`, streams, dependency edges) is in
[`pubsub.md`](./pubsub.md#named-channels). This page covers the
generated code and its runtimes. The gateway is not on the path of a
generated `publish` or `on` call.

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

### TypeScript notes

- The runtime `@gwag/events` is private for now. Depend on it by path
  (`"@gwag/events": "link:../gwag/ts/events"`). It ships TypeScript
  source: run it with Node 24 or newer (type stripping) or a bundler.
- Command:

  ```bash
  gwag gen --lang ts --out src/gen -I proto \
    --opt target=ts --opt import_extension=ts --opt erasable_syntax=true \
    orders/v1/orders.proto
  ```

- Options go to protoc-gen-es as they are. `import_extension=ts` and
  `erasable_syntax=true` make the output run directly under Node
  (`erasable_syntax` turns enums into plain objects).
  `events_import=MODULE` names the module the events file imports the
  runtime from, default `@gwag/events`.
- The proto must import `gw/proto/ps/v1/options.proto` (the long
  path). The generated `_pb.ts` imports its TypeScript from
  `@gwag/events/proto/...`, so no copy of the options file lands next
  to your code. Pass your own `map_imports=...` to change that.

### Python notes

- Setup, then generate:

  ```bash
  uv add betterproto2==0.10.0 nats-py==2.16.0
  uv add --dev betterproto2_compiler==0.10.1
  uv add ./path/to/gwag/py/gwag-events          # import name: gwag_events
  gwag gen --lang python --out proto_gen -I proto --opt paths=source_relative orders/v1/orders.proto
  ```

- The runtime, [`py/gwag-events/`](../py/gwag-events), is not on PyPI.
  Depend on it by path or copy it.
- `--out` must be a Python package: betterproto2 imports
  `from ...message_pool`, so the output directory sits one level above
  the proto package. Import as `proto_gen.orders.v1`.
- betterproto2 writes a module for every file in its request, not only
  the ones you name. `gwag gen` leaves out its own option protos and
  the `descriptor.proto` they pull in. Any other imported proto (for
  example `google/protobuf/timestamp.proto`) is generated too.
- `--opt` values go to betterproto2 unchanged. It ignores the ones it
  does not know.
- Generation fails when a generated name collides with a message or
  enum of the package, with a name the file imports (`Callable`,
  `Message`, ...), or with another channel's, and when a handler
  keyword is taken (`unknown`, `options`, `group`). Rename the channel
  or the message.
- The generated file and the runtime pass `mypy --strict`.
  betterproto2's own `message_pool.py` does not (it calls an untyped
  constructor); the example's `pyproject.toml` relaxes
  `disallow_untyped_calls` for that one module.

## Calls by language

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

### Go

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

- `XSubject` — one exported `string` field per pattern segment.
- `PublishX` options: `events.MsgID(id)` (default random) and
  `events.Header(k, v)`.
- `OnX` options: `events.MaxDeliver`, `events.DeliverAll`,
  `events.Backoff`, `events.AckWait` (see [Delivery](#delivery)).
- `NewBus(backend, events.WithLogf(f))` sets the logger for skipped
  messages (default `log.Printf`).
- Union: `XEvent`, a sealed interface the member messages implement
  (marker method in the generated file), and `XHandlers`.
- Oneof: `XHandlers` with one field per case.
- `events.MessageFrom(ctx)` inside a handler returns the raw message:
  subject, id, headers, delivery attempt.

### TypeScript

Messages are protobuf-es messages; publish takes a message or its init
object (`MessageInitShape`).

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

- `XSubject` — `{ orderId: string }`: one property per pattern segment,
  lower camel case (`{order_id}` is `orderId`, `*` is `p1`, `>` is
  `rest`).
- `publishX(subject, msg, opts?)` — `opts` is `{ msgId?, headers? }`.
- `onX(group, handler, opts?)` — `opts` is `{ maxDeliver?,
  deliverAll?, backoffMs?: number[], ackWaitMs? }`. `stop()` ends this
  subscriber and does not wait for a running handler.
- Handlers get `(subject, msg, raw)`; `raw` is the `Message`: subject,
  id, headers, data, delivery attempt.
- Union: `XEvent` is `{ type: "pkg.Msg"; value: Msg } | ...`,
  `XEventInit` is the same with init objects as values, and
  `XHandlers` has one optional key per message (its protobuf-es name)
  plus `unknown`, which gets `(subject, raw)`.
- Oneof: `XHandlers` has one optional key per case (protobuf-es
  property name) plus `unknown`.
- `new OrdersBus(backend, { logf })` sets the logger for skipped
  messages (default `console.warn`).
- 64-bit integers are `bigint`.

### Python

```python
from gwag_events import JetStreamBackend
from proto_gen.orders.v1 import OrderCreated, OrderShipped
from proto_gen.orders.v1.orders_events import OrderEventsSubject, OrdersBus, OrderShippedSubject

nc = await nats.connect(url)
bus = OrdersBus(JetStreamBackend(nc))

# Plain
await bus.publish_order_shipped(OrderShippedSubject(order_id="o-1"), OrderShipped(carrier="ups"))
stop = await bus.on_order_shipped("notify", handler)   # async def handler(subject, message) -> None
await stop()

# Union: one consumer per (channel, group), dispatch by Gwag-Type
await bus.publish_order_events(OrderEventsSubject(order_id="o-1"), OrderCreated(customer="ann"))
stop = await bus.on_order_events(
    "billing",
    order_created=on_created,      # async def (OrderEventsSubject, OrderCreated) -> None
    order_cancelled=on_cancelled,
    unknown=on_unknown,            # async def (OrderEventsSubject, Message) -> None
)

# Oneof: per case, or the whole message with on_payments_message
stop = await bus.on_payments("ledger", captured=on_captured, note=on_note)
```

- `XSubject` — frozen dataclass, one `str` field per pattern segment,
  in snake case: `{orderId}` is `order_id`, `*` is `p1`, `p2`, ..., a
  trailing `>` is `rest`. A keyword gets a trailing `_` (`from_`).
- `publish_x(subject, message, *, msg_id=None, headers=None)`;
  `msg_id` defaults to a random id.
- `on_x(group, handler, *, options=None) -> Stop`, `Stop` is
  `async () -> None`. `stop()` returns after the current handler
  finishes.
- Union: `XEvent`, a type alias of the member classes, which
  `publish_x` takes. `on_x(group, *, <member>=..., unknown=...,
  options=None)` has one keyword per member, in snake case.
- Oneof: `on_x(group, *, <case>=..., unknown=..., options=None)`. A
  case value is typed from its proto field. A case whose type lives in
  another package, or is a well-known type betterproto2 unwraps, is
  typed `Any`.
- `OrdersBus(backend, logf=f)` sets the logger for skipped messages.
- Durations are seconds: `SubscribeOptions(max_deliver=5,
  deliver_all=False, backoff=(1.0, 5.0, 30.0), ack_wait=30.0)`.

## Delivery

- **Group** — the `group` argument of `On`. Every group gets every
  message. Subscribers sharing a group share its messages: run the
  same service three times and each message reaches one of the three.
  This holds across languages. On JetStream a group is a durable
  consumer, so messages published while no subscriber runs wait for
  the group.
- **New group** — starts at new messages. `DeliverAll` starts it at
  the first stored message. Only the first subscriber's options create
  the group; later subscribers use it as it is.
- **Ack** — the handler succeeds.
- **Redelivery** — the handler fails. The message comes back after the
  backoff, default 1s, 5s, 30s; the last value repeats.
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

## Wire format

| Header | Value |
|---|---|
| `Gwag-Type` | full name of the body message, always set |
| `Gwag-Channel` | channel key |
| `Nats-Msg-Id` | message id on NATS (JetStream dedup) |
| `Gwag-Msg-Id` | message id on other backends |
| `Gwag-Error` | failure text, on dead letters only |

Body: proto binary. The format is the same in all three languages, so
a Go publisher can feed a Python listener and the reverse.

## Backends

All three runtimes have the same three backends: JetStream, memory,
and Kafka as an example to copy.

- **JetStream** — one stream per channel named
  `EV_<package with . as _>_<name>` (`EV_orders_v1_OrderEvents`),
  subject = the pattern with `{x}` as `*`. The backend creates a
  missing stream (file storage, limits retention, MaxAge 7d,
  Duplicates 2m, 1 replica) and never updates an existing one; the
  gateway ensures streams with its cluster policy and wins.
  Republishing a message id inside the 2 minute window stores it once.
  Messages are pulled in batches (`prefetch`, default 16); lower it to
  1 to spread a group's work evenly, as prefetched messages count
  against AckWait.
- **Memory** — in-process, same group and retry semantics, dead
  letters kept for assertions. For tests.
- **Kafka** — topic per channel (stream name), record key = subject,
  consumer group `<stream>.<group>`, retries in process (the
  partition waits), DLQ topic `gwag.dlq.<stream>`. No dedup: Kafka
  does not drop a repeated `Gwag-Msg-Id`. Each `test.sh` runs against
  `apache/kafka` in docker.

| | Go | TypeScript | Python |
|---|---|---|---|
| JetStream | `jetstream.New(nc, jetstream.WithPrefetch(n))` | `new JetStreamBackend(nc, { prefetch?, logf? })` | `JetStreamBackend(nc, prefetch=16, logf=)` |
| Memory | `memory.New()`, `DeadLetters()` | `new MemoryBackend()`, `deadLetters()` | `MemoryBackend()`, `dead_letters()` |
| Backend suite | `events/eventstest.Run` | `runBackendSuite` from `@gwag/events/eventstest` | `gwag_events.testing.CHECKS` |
| Kafka example | [`go-kafka`](../examples/events/go-kafka) (franz-go, own module) | [`ts-kafka`](../examples/events/ts-kafka) (kafkajs) | [`py-kafka`](../examples/events/py-kafka) (aiokafka) |
| Runtime | `events/` (`events/jetstream`, `events/memory`) | [`ts/events/`](../ts/events) | [`py/gwag-events/`](../py/gwag-events) |

`nc` is a `*nats.Conn` (Go), a `@nats-io/transport-node` connection
(TypeScript) or a `nats-py` client (Python). The JetStream backends
behave the same: a missing stream is created and an existing one is
left alone; ack on success, nak with backoff on error, and after
`MaxDeliver` a term plus a publish to `gwag.dlq.<stream>`.

A backend is this interface; it owns delivery (ack on success,
redeliver after the backoff, dead-letter after `MaxDeliver`):

```go
type Backend interface {
    Publish(ctx context.Context, ch events.Channel, m *events.Message) error
    Subscribe(ctx context.Context, ch events.Channel, group string, opts events.SubscribeOptions, h events.Handler) (stop func(), err error)
    Close() error
}
```

The TypeScript and Python `Backend` are the same three methods. Run
the backend suite against any new backend.

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
`controlclient`, so the copy is yours. TypeScript and Python have no
control-plane client; their `subscriptions()` return the same pairs.

## Security

The bus is open by default. Any client that reaches the NATS port can
publish or subscribe on any subject, including these channels and the
DLQ subjects. Message authorship is an application concern: sign
payloads yourself if consumers must trust who sent them. gwag's HMAC
channel auth covers only `ps.pub` and `ps_sub` through the gateway
([`pubsub.md`](./pubsub.md#auth-tiers)), not calls made with generated
code or a raw NATS client.

## Checks

`examples/events/check.sh` regenerates the Go and TypeScript code and
fails when it differs from what is committed, vets and tests the Go
example, runs the TypeScript tests, and then the Python checks.

| Command | Runs |
|---|---|
| `examples/events/check.sh` | everything above; JetStream tests and Go-TypeScript interop need docker and are skipped without it |
| `KAFKA=1 examples/events/check.sh` | also `go-kafka`, `ts-kafka` and `py-kafka` against `apache/kafka` (docker) |
| `PY=0 examples/events/check.sh` | skips Python |
| `examples/events/py/check.sh` | `uv lock --check`, Python regen and drift check, `mypy --strict`, runtime and example tests on an embedded NATS (no docker), including Go-Python interop (`tests/test_interop.py`: Go to Python, Python to Go, one group split across both); `KAFKA=1` adds `py-kafka` |
| `examples/events/interop.sh` | Go publisher into TypeScript listener, the reverse, and a Go and a TypeScript member of one group (docker) |
| `ts/events/test.sh` | TypeScript runtime tests against NATS in docker |

The Kafka scripts take `KAFKA_PORT` (default 19092 for Go and
TypeScript, 19093 for Python) and refuse to start when it is in use.

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
