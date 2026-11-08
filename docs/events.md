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
| `--lang` | `go`, `python` (alias `py`); TypeScript: see below |
| `--out` | output directory, default `.` |
| `-I` | import path, repeatable, default `.` |
| `--opt` | generator parameter, repeatable; passed to every generator (`paths=source_relative`, `module=...`, `M...`) |
| `--grpc` | also run the gRPC stub generator |
| `--module-dir` | project root that pins the generators (`go.mod`, `pyproject.toml`), default `.` |

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
- **python** — `uv run --locked --project <module-dir>
  protoc-gen-python_betterproto2`, from the uv project's own
  environment. `--locked` fails when `uv.lock` is out of date. Pin it
  once:

  ```bash
  uv add --dev betterproto2_compiler==0.10.1
  uv add betterproto2==0.10.0
  ```

  `uv` itself is the only tool taken from `PATH`. If the compiler is
  missing, the error prints the `uv add` line.

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
  its own Go module (franz-go); Python:
  [`examples/events/py-kafka/`](../examples/events/py-kafka) (aiokafka,
  same mapping). Topic per channel (stream name), record
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

TODO (E3): `--lang typescript`, `protoc-gen-es` from the project's
`node_modules/.bin`.

## Python

`gwag gen --lang python` runs betterproto2's plugin for the message
classes and adds `<pkg dir>/<file>_events.py` next to them. Runnable
example, including a Go and a Python service on one NATS:
[`examples/events/py/`](../examples/events/py) (`check.sh` runs it).

### Setup

A uv project that pins the compiler and the runtime:

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

### API

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

The class is `<File>Bus`, named after the `.proto` file (`orders.proto`
gives `OrdersBus`), one per file. Per channel `X` (snake case `x`):

- `XSubject` — frozen dataclass, one `str` field per pattern segment,
  in snake case: `{orderId}` is `order_id`, `*` is `p1`, `p2`, ..., a
  trailing `>` is `rest`. A keyword gets a trailing `_` (`from_`).
- `publish_x(subject, message, *, msg_id=None, headers=None)`.
  `msg_id` defaults to a random id.
- `on_x(group, handler, *, options=None) -> Stop`. `Stop` is
  `async () -> None`.
- Union: `XEvent`, a type alias of the member classes, which
  `publish_x` takes. `on_x(group, *, <member>=..., unknown=...,
  options=None)` has one keyword per member, in snake case.
- Oneof: `on_x(group, *, <case>=..., unknown=..., options=None)` has
  one keyword per case. `on_x_message(group, handler)` takes the whole
  message. A case value is typed from its proto field. A case whose
  type lives in another package, or is a well-known type betterproto2
  unwraps, is typed `Any`.
- A keyword left out acks that type or case without handling it. A
  type with no handler and no `unknown` is acked and logged once per
  (channel, type).
- `message_from()` inside a handler returns the raw `Message`: subject,
  id, headers, delivery attempt.
- `bus.subscriptions()` returns the `(channel, group)` pairs, as in Go.

A handler acks by returning and asks for redelivery by raising.
Durations are seconds: `SubscribeOptions(max_deliver=5,
deliver_all=False, backoff=(1.0, 5.0, 30.0), ack_wait=30.0)`.

Generation fails when a generated name collides with a message or enum
of the package, with a name the file imports (`Callable`, `Message`,
...), or with another channel's, and when a handler keyword is taken
(`unknown`, `options`, `group`). Rename the channel or the message.

The generated file and the runtime pass `mypy --strict`. betterproto2's
own `message_pool.py` does not (it calls an untyped constructor); the
example's `pyproject.toml` relaxes `disallow_untyped_calls` for that one
module.

### Runtime and backends

`gwag_events` mirrors the Go `events` package: a `Backend` protocol
(`publish`, `subscribe`, `close`), the same header names, the same
pattern parse, fill and match, the same stream names.

- **`JetStreamBackend(nc)`** — nats-py. Same stream and consumer
  behaviour as the Go backend: a missing stream is created (file,
  limits, 7d, 2m duplicate window, 1 replica) and an existing one is
  left alone. A group is a durable consumer, used as it is when it
  exists. A new group starts at new messages. Ack on success, nak with
  backoff on error, and after `max_deliver` a term plus a publish to
  `gwag.dlq.<stream>` with the original headers and `Gwag-Error`. A
  subscriber handles its fetched messages one at a time. `prefetch=`
  (default 16) is the fetch batch size; lower it to spread a group's
  work more evenly. `stop()` returns after the current handler
  finishes.
- **`MemoryBackend()`** — in-process, same semantics, `dead_letters()`
  for assertions.
- **Kafka** — [`examples/events/py-kafka/`](../examples/events/py-kafka),
  its own uv project (aiokafka). Same mapping as the Go one: topic per
  channel, record key = subject, group `<stream>.<group>`, retries in
  process, DLQ topic `gwag.dlq.<stream>`, no dedup. `test.sh` runs it
  against `apache/kafka` in docker.
- `gwag_events.testing.CHECKS` is the behaviour suite every backend
  should pass, the Python twin of `events/eventstest`.

Go and Python consumers in one group on one NATS share its messages:
the wire format is the same. `examples/events/py/tests/test_interop.py`
checks Go to Python, Python to Go, and one group split across both.

### Pinning

Every tool is pinned in a `pyproject.toml` and its `uv.lock`, and
`.python-version` pins the interpreter. Nothing reads a global
`protoc`, `pip` or `PATH` entry except `uv`. Commit `uv.lock`; do not
commit `.venv`.

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
