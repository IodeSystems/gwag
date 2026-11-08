// Reads topic corp-orders, which another team produces with Confluent's
// serializer (subject corp-orders-value, message corp.orders.v1.Order).
// Two ways, one per row of docs/events.md "Who produces the topic?":
//
//   node corp/main.ts import TOPIC   # typed code from a pinned copy of the subject
//   node corp/main.ts shadow TOPIC   # typed code from gwag's own copy, go-kafka/corp/shadow/shop/v1/order.proto
//
// gen/ and imported/ are written by gen.sh, not committed. KAFKA_BROKERS
// and SR_URL name the broker and the registry. Prints the same lines as
// the Go example (../../go-kafka/corp).
import { enumToJson } from "@bufbuild/protobuf";
import { KafkaBackend, topicTable } from "@gwag/events-kafka";
import { RegistryClient, SchemaRegistrySerde, HeaderSchemaID } from "@gwag/events-schemaregistry";
import { CorpOrdersValueBus } from "./gen/registry/corp-orders-value_events.ts";
import { Order_StatusSchema } from "./gen/registry/corp-orders-value_pb.ts";
import { OrderBus } from "./gen/shop/v1/order_events.ts";

const want = 2;
const [mode, topic] = process.argv.slice(2);
if ((mode !== "import" && mode !== "shadow") || !topic) {
  console.error("usage: corp import|shadow TOPIC");
  process.exit(1);
}
const brokers = (process.env.KAFKA_BROKERS ?? "").split(",");
const registry = new RegistryClient({ url: process.env.SR_URL ?? "" });

const lines: string[] = [];
let finish: () => void;
const done = new Promise<void>((r) => (finish = r));
const got = (line: string) => {
  lines.push(line);
  if (lines.length === want) finish();
};
const keyOf = (raw: { key?: Uint8Array }) => Buffer.from(raw.key ?? []).toString();

let backend: KafkaBackend;
if (mode === "import") {
  // The channel key is <package>.<message name>; the topic is the
  // team's, so the mapper names it.
  backend = new KafkaBackend(brokers, { topics: topicTable({ "corp.orders.v1.Order": { name: topic } }) });
  const bus = new CorpOrdersValueBus(backend, { serde: new SchemaRegistrySerde(registry) });
  await bus.onOrder(
    "corp-import",
    (_s, m, raw) =>
      got(
        `import ${raw.headers["Gwag-Type"]} ${m.orderId} total ${m.total?.cents} ${m.total?.currency} status ${enumToJson(Order_StatusSchema, m.status)} skus [${m.skus.join(" ")}] (key ${keyOf(raw)}, schema id ${raw.headers[HeaderSchemaID]})`,
      ),
    { deliverAll: true },
  );
} else {
  backend = new KafkaBackend(brokers, { topics: topicTable({ "shop.v1.CorpOrders": { name: topic } }) });
  // Records are framed as corp.orders.v1.Order; the handler takes them
  // as the copy.
  const serde = new SchemaRegistrySerde(registry, { typeNames: { "corp.orders.v1.Order": "shop.v1.CorpOrder" } });
  const bus = new OrderBus(backend, { serde });
  await bus.onCorpOrders(
    "corp-shadow",
    (_s, m, raw) => got(`shadow ${raw.headers["Gwag-Type"]} ${m.orderId} total ${m.total?.cents} (key ${keyOf(raw)})`),
    { deliverAll: true },
  );
}

const timeout = setTimeout(() => {
  console.error(`timed out; got ${JSON.stringify(lines)}`);
  process.exit(1);
}, 60000);
await done;
clearTimeout(timeout);
for (const l of lines.sort()) console.log(l);
await backend.close();
