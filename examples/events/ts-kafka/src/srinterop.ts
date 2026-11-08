// Publishes and reads the order channels over Kafka with Schema
// Registry framing, for ../sr-test.sh to check against Confluent's own
// serializer and deserializer and against the Go srinterop
// (../../go-kafka/srinterop). Same output lines.
//
//   node src/srinterop.ts produce SHIPPED_TOPIC EVENTS_TOPIC [lookup]
//   node src/srinterop.ts consume SHIPPED_TOPIC EVENTS_TOPIC N
//
// KAFKA_BROKERS and SR_URL name the broker and the registry. produce
// registers orders.proto under <topic>-value on first publish (own);
// with "lookup" it only looks the schema up (the default serde) and
// fails when it is not registered. consume reads from the start
// of both topics and prints N lines; the records need not carry gwag
// headers.
import { KafkaBackend, topicTable } from "@gwag/events-kafka";
import { HeaderSchemaID, RegistryClient, SchemaRegistrySerde } from "@gwag/events-schemaregistry";
import { OrdersBus } from "../../ts/src/gen/orders/v1/orders_events.ts";

const [cmd, shipped, evs, arg] = process.argv.slice(2);
if (!cmd || !shipped || !evs) {
  console.error("usage: srinterop produce|consume SHIPPED_TOPIC EVENTS_TOPIC [N|lookup]");
  process.exit(1);
}
const brokers = (process.env.KAFKA_BROKERS ?? "").split(",");
const url = process.env.SR_URL ?? "";
const key = (r: { key: Uint8Array | null }) => (r.key ? Buffer.from(r.key).toString() : "j");

const backend = new KafkaBackend(brokers, {
  partitions: 1,
  topics: topicTable({
    // A producer that is not gwag keys records by order id and sends
    // no Gwag-* headers; subject rebuilds the subject.
    "orders.v1.OrderShipped": { name: shipped, subject: (r) => `events.orders.${key(r)}.shipped` },
    "orders.v1.OrderEvents": { name: evs, subject: (r) => `events.orders.${key(r)}.lifecycle` },
  }),
});
const registry = new RegistryClient({ url });

try {
  if (cmd === "produce") {
    const serde = new SchemaRegistrySerde(registry, { autoRegister: arg !== "lookup" });
    const bus = new OrdersBus(backend, { serde });
    await bus.publishOrderShipped({ orderId: "o-1001" }, { orderId: "o-1001", carrier: "ups" });
    await bus.publishOrderEvents({ orderId: "o-1001" }, { type: "orders.v1.OrderCreated", value: { orderId: "o-1001", customer: "ann" } });
    console.log("produced 2");
  } else if (cmd === "consume") {
    await consume(new OrdersBus(backend, { serde: new SchemaRegistrySerde(registry) }), Number(arg));
  } else {
    throw new Error(`unknown command ${cmd}`);
  }
} finally {
  await backend.close();
}

async function consume(bus: OrdersBus, n: number): Promise<void> {
  const lines: string[] = [];
  let finish: () => void;
  const done = new Promise<void>((r) => (finish = r));
  const got = (line: string) => {
    lines.push(line);
    if (lines.length === n) finish();
  };
  const source = (raw: { headers: Record<string, string> }) =>
    raw.headers["Gwag-Channel"] ? "Gwag-Type header" : `schema id ${raw.headers[HeaderSchemaID]}`;
  await bus.onOrderShipped(
    "ts-srinterop",
    (_s, m, raw) =>
      got(`OrderShipped ${m.orderId} via ${m.carrier} (key ${Buffer.from(raw.key ?? []).toString()}, schema id ${raw.headers[HeaderSchemaID]}, gwag headers ${Boolean(raw.headers["Gwag-Channel"])})`),
    { deliverAll: true },
  );
  await bus.onOrderEvents(
    "ts-srinterop",
    {
      OrderCreated: (_s, m, raw) => got(`OrderCreated ${m.orderId} by ${m.customer} (type from ${source(raw)})`),
      OrderCancelled: (_s, m, raw) => got(`OrderCancelled ${m.orderId}: ${m.reason} (type from ${source(raw)})`),
      unknown: (_s, raw) => got(`Unknown ${raw.headers["Gwag-Type"]}`),
    },
    { deliverAll: true },
  );
  const timeout = setTimeout(() => {
    console.error(`timed out; got ${JSON.stringify(lines)}`);
    process.exit(1);
  }, 60000);
  await done;
  clearTimeout(timeout);
  for (const l of lines.sort()) console.log(l);
}
