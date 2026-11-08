// Tests of the generated TypeScript API (src/gen) over the memory backend.
import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import * as pb from "@bufbuild/protobuf";
import { channel, messageType, type Message } from "@gwag/events";
import { MemoryBackend } from "@gwag/events/memory";
import { AuditEntrySchema, OrderCreatedSchema } from "../src/gen/orders/v1/orders_pb.ts";
import { OrdersBus } from "../src/gen/orders/v1/orders_events.ts";

const channelOfOrderEvents = channel("orders.v1.OrderEvents", "events.orders.{orderId}.lifecycle");
const sleep = (ms: number) => new Promise<void>((r) => setTimeout(r, ms));

class Log {
  got: string[] = [];
  add(s: string) {
    this.got.push(s);
  }
  async wait(n: number): Promise<string[]> {
    for (let i = 0; i < 1000 && this.got.length < n; i++) await sleep(5);
    assert.ok(this.got.length >= n, `got ${this.got.length} of ${n}: ${this.got}`);
    await sleep(20);
    return this.got;
  }
}

let mem: MemoryBackend;
function newBus(logs: string[] = []) {
  mem = new MemoryBackend();
  return new OrdersBus(mem, { logf: (m) => logs.push(m) });
}
afterEach(async () => {
  await mem.close();
});

test("plain channel", async () => {
  const bus = newBus();
  const l = new Log();
  await bus.onOrderShipped("notify", (s, m, raw) => {
    l.add(`${s.orderId} ${m.carrier} ${raw.subject} ${messageType(raw)}`);
  });
  await bus.publishOrderShipped({ orderId: "o1" }, { orderId: "o1", carrier: "ups" });
  assert.deepEqual(await l.wait(1), ["o1 ups events.orders.o1.shipped orders.v1.OrderShipped"]);
  await assert.rejects(bus.publishOrderShipped({ orderId: "a.b" }, {}));
});

test("oneof channel", async () => {
  const bus = newBus();
  const l = new Log();
  await bus.onPayments("ledger", {
    captured: (s, m) => l.add(`captured ${s.region}/${s.p1} ${m.orderId}`),
    note: (_s, note) => l.add(`note ${note}`),
    // refunded is missing: acked unhandled.
    unknown: () => l.add("unknown"),
  });
  const s = { region: "eu", p1: "card" };
  await bus.publishPayments(s, { kind: { case: "captured", value: { orderId: "o1" } } });
  await bus.publishPayments(s, { kind: { case: "refunded", value: { orderId: "o1" } } });
  await bus.publishPayments(s, { kind: { case: "note", value: "hi" } });
  await bus.publishPayments(s, {});
  assert.deepEqual(await l.wait(3), ["captured eu/card o1", "note hi", "unknown"]);
});

test("union channel, handler map", async () => {
  const logs: string[] = [];
  const bus = newBus(logs);
  const l = new Log();
  await bus.onOrderEvents("billing", {
    OrderCreated: (s, m) => l.add(`created ${s.orderId} ${m.customer}`),
    OrderCancelled: (s, m) => l.add(`cancelled ${s.orderId} ${m.reason}`),
  });
  const s = { orderId: "o7" };
  await bus.publishOrderEvents(s, { type: "orders.v1.OrderCreated", value: { orderId: "o7", customer: "ann" } });
  await bus.publishOrderEvents(s, { type: "orders.v1.OrderCancelled", value: { orderId: "o7", reason: "late" } });
  // A type this code does not know: acked, logged once.
  for (let i = 0; i < 2; i++) {
    await bus.core.publish(
      channelOfOrderEvents,
      ["o7"],
      "orders.v1.AuditEntry",
      pb.toBinary(AuditEntrySchema, pb.create(AuditEntrySchema, { actor: "x" })),
    );
  }
  await bus.publishOrderEvents(s, { type: "orders.v1.OrderCreated", value: { orderId: "o7", customer: "bob" } });
  assert.deepEqual(await l.wait(3), ["created o7 ann", "cancelled o7 late", "created o7 bob"]);
  assert.equal(logs.length, 1, "unknown type logged once");
});

test("union channel, one function gets a tagged event", async () => {
  const bus = newBus();
  const l = new Log();
  await bus.onOrderEvents("all", (s, e) => {
    switch (e.type) {
      case "orders.v1.OrderCreated":
        l.add(`${s.orderId} created by ${e.value.customer}`);
        break;
      case "orders.v1.OrderCancelled":
        l.add(`${s.orderId} cancelled: ${e.value.reason}`);
        break;
    }
  });
  await bus.publishOrderEvents({ orderId: "o1" }, { type: "orders.v1.OrderCreated", value: pb.create(OrderCreatedSchema, { customer: "ann" }) });
  assert.deepEqual(await l.wait(1), ["o1 created by ann"]);
});

test("union unknown handler", async () => {
  const bus = newBus();
  const l = new Log();
  await bus.onOrderEvents("audit", {
    unknown: (_s, raw: Message) => {
      l.add(`${messageType(raw)} ${pb.fromBinary(AuditEntrySchema, raw.data).actor}`);
    },
  });
  await bus.core.publish(channelOfOrderEvents, ["o1"], "orders.v1.AuditEntry", pb.toBinary(AuditEntrySchema, pb.create(AuditEntrySchema, { actor: "eve" })));
  assert.deepEqual(await l.wait(1), ["orders.v1.AuditEntry eve"]);
});

test("rest segment and subscriptions", async () => {
  const bus = newBus();
  const l = new Log();
  await bus.onAudit("archive", (s, m) => l.add(`${s.rest} ${m.action}`));
  await bus.onOrderShipped("notify", () => {});
  await bus.publishAudit({ rest: "o1.refund" }, { action: "refund" });
  assert.deepEqual(await l.wait(1), ["o1.refund refund"]);
  assert.deepEqual(bus.subscriptions(), [
    { channel: "orders.v1.Audit", group: "archive" },
    { channel: "orders.v1.OrderShipped", group: "notify" },
  ]);
});

test("a failing handler is redelivered, then dead-lettered", async () => {
  const bus = newBus();
  let calls = 0;
  await bus.onOrderShipped(
    "flaky",
    () => {
      calls++;
      throw new Error("nope");
    },
    { maxDeliver: 3, backoffMs: [1] },
  );
  await bus.publishOrderShipped({ orderId: "o1" }, {});
  for (let i = 0; i < 1000 && mem.deadLetters().length === 0; i++) await sleep(5);
  const d = mem.deadLetters()[0]!;
  assert.equal(d.subject, "gwag.dlq.EV_orders_v1_OrderShipped");
  assert.equal(d.headers["Gwag-Error"], "nope");
  assert.equal(calls, 3);
});
