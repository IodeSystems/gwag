// One side of the Go / TypeScript interop check (examples/events/interop.sh).
//
//   interop.ts listen         # group "interop": print the six sample events, then exit
//   interop.ts publish        # publish the six sample events
//   interop.ts listen-shared     # group "shared": print one "shared <id>" line per delivery until SIGTERM
//   interop.ts publish-many N    # publish N OrderShipped events
//
// NATS_URL defaults to nats://127.0.0.1:4222. The Go counterpart is
// examples/events/go-interop; both print the same lines.
import { JetStreamBackend } from "@gwag/events/jetstream";
import { connect } from "@nats-io/transport-node";
import { OrdersBus } from "./gen/orders/v1/orders_events.ts";

const [mode, arg] = process.argv.slice(2);
const nc = await connect({ servers: process.env.NATS_URL ?? "nats://127.0.0.1:4222" });
// Prefetch 1 spreads a group's work evenly across subscribers.
const backend = new JetStreamBackend(nc, { prefetch: 1 });
const bus = new OrdersBus(backend);

async function waitFor(n: number, lines: string[]): Promise<void> {
  const end = Date.now() + 30000;
  while (lines.length < n) {
    if (Date.now() > end) {
      console.error(`timed out: got ${lines.length} of ${n}`);
      process.exit(1);
    }
    await new Promise((r) => setTimeout(r, 10));
  }
}

switch (mode) {
  case "listen": {
    const lines: string[] = [];
    const got = (l: string) => void lines.push(l);
    await bus.onOrderEvents("interop", {
      OrderCreated: (s, m) => got(`OrderEvents ${s.orderId} created by ${m.customer}`),
      OrderCancelled: (s, m) => got(`OrderEvents ${s.orderId} cancelled: ${m.reason}`),
    });
    await bus.onPayments("interop", {
      captured: (s, m) => got(`Payments ${s.region}/${s.p1} captured ${m.amountCents} for ${m.orderId}`),
      refunded: (s, m) => got(`Payments ${s.region}/${s.p1} refunded ${m.amountCents}: ${m.reason}`),
      note: (s, note) => got(`Payments ${s.region}/${s.p1} note ${note}`),
    });
    await bus.onOrderShipped("interop", (s, m) => got(`OrderShipped ${s.orderId} via ${m.carrier}`));
    await bus.onAudit("interop", (s, m) => got(`Audit ${s.rest}: ${m.actor} ${m.action}`));
    console.log("ready");
    await waitFor(7, lines);
    for (const l of lines.sort()) console.log(l);
    break;
  }
  case "publish": {
    await bus.publishOrderEvents({ orderId: "o-1001" }, { type: "orders.v1.OrderCreated", value: { orderId: "1001", customer: "ann" } });
    await bus.publishOrderEvents({ orderId: "o-1002" }, { type: "orders.v1.OrderCancelled", value: { orderId: "1002", reason: "out of stock" } });
    await bus.publishPayments({ region: "eu", p1: "card" }, { kind: { case: "captured", value: { orderId: "o-1001", amountCents: 4200n } } });
    await bus.publishPayments({ region: "eu", p1: "card" }, { kind: { case: "refunded", value: { orderId: "o-1002", amountCents: 1500n, reason: "cancelled" } } });
    await bus.publishPayments({ region: "us", p1: "wire" }, { kind: { case: "note", value: "hello" } });
    await bus.publishOrderShipped({ orderId: "o-1001" }, { orderId: "o-1001", carrier: "ups" });
    await bus.publishAudit({ rest: "o-1002.refund" }, { actor: "support", action: "refund" });
    break;
  }
  case "listen-shared": {
    await bus.onOrderShipped("shared", async (_s, m) => {
      await new Promise((r) => setTimeout(r, 5));
      console.log(`shared ${m.orderId}`);
    });
    console.log("ready");
    // Runs until the script stops it.
    await new Promise<void>((resolve) => process.on("SIGTERM", resolve));
    break;
  }
  case "publish-many": {
    const n = Number(arg);
    for (let i = 0; i < n; i++) await bus.publishOrderShipped({ orderId: `k${i}` }, { orderId: `ts-${i}`, carrier: "ups" });
    break;
  }
  default:
    console.error("usage: interop.ts listen | publish | listen-shared | publish-many N");
    process.exit(2);
}

await backend.close();
await nc.close();
