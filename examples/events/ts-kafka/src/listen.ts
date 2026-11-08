// Listens on every order channel over Kafka as one consumer group,
// with the Schema Registry serde, and prints one line per delivery
// until SIGTERM. The e2e story e2e/s12_kafka_events_test.go runs it;
// the lines are those of ../../ts/src/interop.ts listen-group.
//
//   node src/listen.ts GROUP
//
// KAFKA_BROKERS and SR_URL name the broker and the registry. Topics are
// the backend defaults (one per channel, EV_<pkg>_<Name>).
import { KafkaBackend } from "@gwag/events-kafka";
import { RegistryClient, SchemaRegistrySerde } from "@gwag/events-schemaregistry";
import { OrdersBus } from "../../ts/src/gen/orders/v1/orders_events.ts";

const group = process.argv[2];
if (!group) {
  console.error("usage: listen.ts GROUP");
  process.exit(2);
}
const backend = new KafkaBackend((process.env.KAFKA_BROKERS ?? "").split(","));
const bus = new OrdersBus(backend, { serde: new SchemaRegistrySerde(new RegistryClient({ url: process.env.SR_URL ?? "" })) });

const out = (l: string) => void console.log(l);
await bus.onOrderEvents(group, {
  OrderCreated: (s, m) => out(`OrderEvents ${s.orderId} created by ${m.customer}`),
  OrderCancelled: (s, m) => out(`OrderEvents ${s.orderId} cancelled: ${m.reason}`),
});
await bus.onPayments(group, {
  captured: (s, m) => out(`Payments ${s.region}/${s.p1} captured ${m.amountCents} for ${m.orderId}`),
  refunded: (s, m) => out(`Payments ${s.region}/${s.p1} refunded ${m.amountCents}: ${m.reason}`),
  note: (s, note) => out(`Payments ${s.region}/${s.p1} note ${note}`),
});
await bus.onOrderShipped(group, (s, m) => out(`OrderShipped ${s.orderId} via ${m.carrier}`));
await bus.onAudit(group, (s, m) => out(`Audit ${s.rest}: ${m.actor} ${m.action}`));
console.log("ready");
await new Promise<void>((resolve) => process.on("SIGTERM", resolve));
await backend.close();
