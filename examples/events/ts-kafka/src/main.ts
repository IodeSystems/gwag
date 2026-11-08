// Runs the generated order bus over Kafka: the same typed calls as
// ../../ts/src/main.ts, another Backend. The generated code is the
// TypeScript example's (../../ts/src/gen).
//
//   KAFKA_BROKERS=localhost:9092 pnpm start
//
// ../test.sh starts apache/kafka in docker and runs this and the
// @gwag/events-kafka tests (ts/events-kafka).
import { OrdersBus } from "../../ts/src/gen/orders/v1/orders_events.ts";
import { KafkaBackend } from "@gwag/events-kafka";

const brokers = process.env.KAFKA_BROKERS;
if (!brokers) {
  console.error("set KAFKA_BROKERS (e.g. localhost:9092)");
  process.exit(1);
}
const backend = new KafkaBackend(brokers.split(","));
const bus = new OrdersBus(backend);

const lines: string[] = [];
let pending = 4;
let finish: () => void;
const done = new Promise<void>((r) => (finish = r));
const got = (line: string) => {
  lines.push(line);
  if (--pending === 0) finish();
};

await bus.onOrderEvents("billing", {
  OrderCreated: (s, m) => got(`OrderEvents   ${s.orderId} created by ${m.customer}`),
  OrderCancelled: (s, m) => got(`OrderEvents   ${s.orderId} cancelled: ${m.reason}`),
});
await bus.onPayments("ledger", {
  captured: (s, m) => got(`Payments      ${s.region}/${s.p1} captured ${m.amountCents} for ${m.orderId}`),
});
await bus.onOrderShipped("notify", (s, m) => got(`OrderShipped  ${s.orderId} via ${m.carrier}`));
// A new group starts at new messages; give the consumers a moment to
// resolve their start offsets.
await new Promise((r) => setTimeout(r, 1000));

await bus.publishOrderEvents({ orderId: "o-1001" }, { type: "orders.v1.OrderCreated", value: { orderId: "o-1001", customer: "ann" } });
await bus.publishOrderEvents({ orderId: "o-1002" }, { type: "orders.v1.OrderCancelled", value: { orderId: "o-1002", reason: "out of stock" } });
await bus.publishPayments({ region: "eu", p1: "card" }, { kind: { case: "captured", value: { orderId: "o-1001", amountCents: 4200n } } });
await bus.publishOrderShipped({ orderId: "o-1001" }, { orderId: "o-1001", carrier: "ups" });

const timeout = setTimeout(() => {
  console.error("timed out waiting for deliveries");
  process.exit(1);
}, 30000);
await done;
clearTimeout(timeout);
for (const l of lines.sort()) console.log(l);
await backend.close();
