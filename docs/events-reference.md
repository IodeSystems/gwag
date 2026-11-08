# Typed events: reference

Per-language generator and call details, runtime packages, writing a
backend, checks, adding a language. Start at
[`events.md`](./events.md); Kafka and Schema Registry are in
[`events-kafka.md`](./events-kafka.md).

## Runtime packages

| | Go | TypeScript | Python |
|---|---|---|---|
| Runtime | `events/` (`events/jetstream`, `events/memory`) | [`ts/events/`](../ts/events) (`@gwag/events`) | [`py/gwag-events/`](../py/gwag-events) (`gwag_events`) |
| Backend suite | `events/eventstest.Run` | `runBackendSuite` from `@gwag/events/eventstest` | `gwag_events.testing.CHECKS` |
| Kafka | [`events/kafka`](../events/kafka) | [`ts/events-kafka/`](../ts/events-kafka) (`@gwag/events-kafka`) | [`py/gwag-events-kafka/`](../py/gwag-events-kafka) (`gwag_events_kafka`) |
| Schema Registry | [`events/schemaregistry`](../events/schemaregistry) | [`ts/events-schemaregistry/`](../ts/events-schemaregistry) (`@gwag/events-schemaregistry`) | [`py/gwag-events-schemaregistry/`](../py/gwag-events-schemaregistry) (`gwag_events_schemaregistry`) |

The TypeScript and Python packages are private (not on npm or PyPI).
Depend on them by path.

## TypeScript setup

- `@gwag/events` ships TypeScript source: run it with Node 24 or newer
  (type stripping) or a bundler. Depend on it by path
  (`"@gwag/events": "link:../gwag/ts/events"`).
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
  `@gwag/events/proto/...`, so no options file lands next
  to your code. Pass your own `map_imports=...` to change that.

## Python setup

- Setup, then generate:

  ```bash
  uv add betterproto2==0.10.0 nats-py==2.16.0
  uv add --dev betterproto2_compiler==0.10.1
  uv add ./path/to/gwag/py/gwag-events          # import name: gwag_events
  gwag gen --lang python --out proto_gen -I proto --opt paths=source_relative orders/v1/orders.proto
  ```

- Kafka and the Schema Registry are separate packages, so
  `gwag-events` has no Kafka or HTTP dependency:
  `uv add ./path/to/gwag/py/gwag-events-kafka` (`gwag_events_kafka`)
  and `uv add ./path/to/gwag/py/gwag-events-schemaregistry`
  (`gwag_events_schemaregistry`). `Message.key` is the Kafka record
  key as bytes.
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
- The generated file also registers the serialized
  `FileDescriptorProto` of its proto file and of each non-builtin
  import (`register_file_descriptors`). betterproto2 messages carry no
  descriptor, and the Schema Registry serde needs one to register a
  schema and to find a message's index in its file. They are the bytes
  the Go runtime registers, so both register one schema.

## Calls per language

### Go

`<file>.events.go` lands in the same package as `<file>.pb.go` (the
proto's `go_package`). Oneof channel, per case or whole message:

```go
stop, err = bus.OnPayments(ctx, "ledger", ordersv1.PaymentsHandlers{
    Captured: func(ctx context.Context, s ordersv1.PaymentsSubject, m *ordersv1.PaymentCaptured) error { return nil },
})
// whole message: bus.OnPaymentsMessage(ctx, group, fn, opts...)
```

- `XSubject` — one exported `string` field per pattern segment.
- `PublishX` options: `events.MsgID(id)` (default random) and
  `events.Header(k, v)`.
- `OnX` options: `events.MaxDeliver`, `events.DeliverAll`,
  `events.Backoff`, `events.AckWait` (see
  [Delivery](./events.md#delivery)).
- `NewBus(backend, events.WithLogf(f))` sets the logger for skipped
  messages (default `log.Printf`). `events.WithSerde(s)` sets the wire
  format.
- Union: `XEvent`, a sealed interface the member messages implement
  (marker method in the generated file), and `XHandlers`.
- Oneof: `XHandlers` with one field per case.
- `events.MessageFrom(ctx)` inside a handler returns the raw message:
  subject, id, headers, delivery attempt, record key (Kafka).

### TypeScript

Messages are protobuf-es messages; publish takes a message or its init
object (`MessageInitShape`).

```ts
// Union: one function for all types
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
  id, headers, data, delivery attempt, key (Kafka).
- Union: `XEvent` is `{ type: "pkg.Msg"; value: Msg } | ...`,
  `XEventInit` is the same with init objects as values, and
  `XHandlers` has one optional key per message (its protobuf-es name)
  plus `unknown`, which gets `(subject, raw)`.
- Oneof: `XHandlers` has one optional key per case (protobuf-es
  property name) plus `unknown`.
- `new OrdersBus(backend, { logf, serde })` sets the logger for skipped
  messages (default `console.warn`) and the wire format (default plain
  proto).
- 64-bit integers are `bigint`.

### Python

```python
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
- `OrdersBus(backend, *, serde=None, logf=None)`: `serde` is a
  `gwag_events.Serde` (default plain proto), `logf` the logger for
  skipped messages.
- `message_from()` inside a handler returns the delivered `Message`
  (or `None` outside one).
- Durations are seconds: `SubscribeOptions(max_deliver=5,
  deliver_all=False, backoff=(1.0, 5.0, 30.0), ack_wait=30.0)`.

## Writing a backend

A backend owns delivery: ack on success, redeliver after the backoff,
dead-letter after `MaxDeliver`.

```go
type Backend interface {
    Publish(ctx context.Context, ch events.Channel, m *events.Message) error
    Subscribe(ctx context.Context, ch events.Channel, group string, opts events.SubscribeOptions, h events.Handler) (stop func(), err error)
    Close() error
}
```

The TypeScript and Python `Backend` are the same three methods. Run
the backend suite (table above) against a new backend.

A backend may also implement `Topics` (all three languages) to name
the topic of a channel; the serde receives that name.

## Checks

`examples/events/check.sh` regenerates the Go and TypeScript code and
fails when it differs from what is committed, vets and tests the Go
example, runs the TypeScript tests, and then the Python checks.

| Command | Runs |
|---|---|
| `examples/events/check.sh` | everything above; JetStream tests and Go-TypeScript interop need docker and are skipped without it |
| `KAFKA=1 examples/events/check.sh` | also `go-kafka`, `ts-kafka` (`test.sh` and `sr-test.sh`) and `py-kafka` against `apache/kafka` (docker); `go-kafka`, `ts-kafka/sr-test.sh` and `py-kafka` also start `cp-schema-registry`; `go-kafka` and `ts-kafka/sr-test.sh` need `uv`, `py-kafka` needs `uv` and `go` (`SR=0` skips that part of `go-kafka` and `py-kafka`) |
| `PY=0 examples/events/check.sh` | skips Python |
| `examples/events/py/check.sh` | `uv lock --check`, Python regen and drift check, `mypy --strict`, runtime, Kafka-backend (skipped without a broker), serde and example tests on an embedded NATS (no docker), including Go-Python interop (`tests/test_interop.py`: Go to Python, Python to Go, one group split across both); `KAFKA=1` adds `py-kafka` (backend tests, Schema Registry interop) |
| `examples/events/interop.sh` | Go publisher into TypeScript listener, the reverse, and a Go and a TypeScript member of one group (docker) |
| `ts/events/test.sh` | TypeScript runtime tests against NATS in docker |
| `ts/events-kafka/test.sh` | `@gwag/events-kafka` tests against Kafka in docker |
| `examples/events/ts-kafka/sr-test.sh` | `@gwag/events-schemaregistry` against Kafka and Schema Registry in docker: Go, Confluent (Java, Python) and TypeScript read each other; import and shadow |

The Kafka scripts take `KAFKA_PORT` (default 19092 for Go and
TypeScript, 19093 for Python) and refuse to start when it is in use.
The registry scripts also take `SR_PORT` (18081 for Go and TypeScript,
18083 for Python).

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
