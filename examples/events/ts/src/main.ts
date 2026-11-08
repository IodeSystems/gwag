// Publishes and handles the sample order events over NATS JetStream
// with the generated TypeScript bus. Prints the same lines as
// examples/events/go-jetstream.
//
//   NATS_URL=nats://127.0.0.1:4222 pnpm start
//
// NATS_URL defaults to nats://127.0.0.1:4222. Against a gwag cluster,
// point it at a gateway's NATS client port (gwag up: 14222).
import { JetStreamBackend } from "@gwag/events/jetstream";
import { connect } from "@nats-io/transport-node";
import { OrdersBus } from "./gen/orders/v1/orders_events.ts";

const nc = await connect({ servers: process.env.NATS_URL ?? "nats://127.0.0.1:4222" });
const backend = new JetStreamBackend(nc);
const bus = new OrdersBus(backend);

const lines: string[] = [];
let pending = 6;
let finish: () => void;
const done = new Promise<void>((r) => (finish = r));
const got = (line: string) => {
  lines.push(line);
  if (--pending === 0) finish();
};

// Each on* call is one durable consumer per (channel, group). Replicas
// of this service calling it with the same group share the messages.
await bus.onOrderEvents("billing", {
  OrderCreated: (s, m) => got(`OrderEvents   ${s.orderId} created by ${m.customer}`),
  OrderCancelled: (s, m) => got(`OrderEvents   ${s.orderId} cancelled: ${m.reason}`),
});
await bus.onPayments("ledger", {
  captured: (s, m) => got(`Payments      ${s.region}/${s.p1} captured ${m.amountCents} for ${m.orderId}`),
  refunded: (s, m) => got(`Payments      ${s.region}/${s.p1} refunded ${m.amountCents}: ${m.reason}`),
});
await bus.onOrderShipped("notify", (s, m) => got(`OrderShipped  ${s.orderId} via ${m.carrier}`));
await bus.onAudit("archive", (s, m) => got(`Audit         ${s.rest}: ${m.actor} ${m.action}`));

// What a service passes to the control plane as its subscriptions.
for (const s of bus.subscriptions()) console.log(`subscribes    ${s.channel} as ${s.group}`);

await bus.publishOrderEvents({ orderId: "o-1001" }, { type: "orders.v1.OrderCreated", value: { orderId: "o-1001", customer: "ann" } });
await bus.publishPayments({ region: "eu", p1: "card" }, { kind: { case: "captured", value: { orderId: "o-1001", amountCents: 4200n } } });
await bus.publishOrderShipped({ orderId: "o-1001" }, { orderId: "o-1001", carrier: "ups" });
await bus.publishOrderEvents({ orderId: "o-1002" }, { type: "orders.v1.OrderCancelled", value: { orderId: "o-1002", reason: "out of stock" } });
await bus.publishPayments({ region: "eu", p1: "card" }, { kind: { case: "refunded", value: { orderId: "o-1002", amountCents: 1500n, reason: "cancelled" } } });
await bus.publishAudit({ rest: "o-1002.refund" }, { actor: "support", action: "refund" });

const timeout = setTimeout(() => {
  console.error("timed out waiting for deliveries");
  process.exit(1);
}, 10000);
await done;
clearTimeout(timeout);
for (const l of lines.sort()) console.log(l);

await backend.close();
await nc.close();
