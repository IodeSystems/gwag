// Mapper, retry-topic and foreign-record checks. Need Kafka:
// KAFKA_BROKERS=127.0.0.1:19092 (../test.sh sets it). Without it the
// Kafka part is skipped, and says so.
import assert from "node:assert/strict";
import { test } from "node:test";
import { channel, Core, newID, resolveSubscribeOptions, type Message } from "@gwag/events";
import { Kafka, logLevel } from "kafkajs";
import { KafkaBackend, retryTopic, topicTable } from "../src/kafka.ts";

const enc = new TextEncoder();
const sleep = (ms: number) => new Promise<void>((r) => setTimeout(r, ms));
const brokers = process.env.KAFKA_BROKERS?.split(",");

test("topic names and defaults need no broker", () => {
  const ch = channel("orders.v1.OrderEvents", "events.orders.{order_id}.lifecycle");
  const lit = channel("corp.orders.v1.Order", "corp-orders");
  const b = new KafkaBackend(["127.0.0.1:1"], {
    topics: topicTable({ "orders.v1.OrderEvents": { name: "orders.lifecycle.v1", group: (g) => `shop-${g}`, dlq: "dead", partitions: 12 } }),
  });
  assert.equal(b.topic(ch), "orders.lifecycle.v1");
  const t = b.topicOf(ch);
  assert.equal(t.group("billing"), "shop-billing");
  assert.equal(t.dlq, "dead");
  assert.equal(t.partitions, 12);
  assert.equal(t.retry("billing"), "gwag.retry.orders.lifecycle.v1.billing");
  const d = b.topicOf(lit);
  assert.equal(d.name, "EV_corp_orders_v1_Order");
  assert.equal(d.dlq, "gwag.dlq.EV_corp_orders_v1_Order");
  assert.equal(d.partitions, 3);
  assert.equal(d.replication, 1);
  const rec = { topic: "t", partition: 0, offset: "0", key: Buffer.from("o-1"), value: null, headers: {} };
  assert.equal(d.subject(rec), "corp-orders", "literal pattern");
  assert.equal(b.topicOf(ch).subject(rec), "o-1", "wildcard pattern: the key");
});

test("resolved options carry the retry mode", () => {
  assert.equal(resolveSubscribeOptions(retryTopic()).backend["kafka.retry"], "topic");
});

if (!brokers) {
  test("kafka mapping", { skip: "KAFKA_BROKERS not set; run ../test.sh" }, () => {});
} else {
  const run = newID().slice(0, 8);
  const kafka = new Kafka({ brokers, logLevel: logLevel.NOTHING });
  type Rec = { key: string; headers: Record<string, string>; value: Buffer };

  async function readAll(topic: string, n: number): Promise<Rec[]> {
    const admin = kafka.admin();
    await admin.connect();
    for (let i = 0; i < 300 && !(await admin.listTopics()).includes(topic); i++) await sleep(100);
    await admin.disconnect();
    const c = kafka.consumer({ groupId: `reader-${newID()}` });
    await c.connect();
    await c.subscribe({ topic, fromBeginning: true });
    const out: Rec[] = [];
    await c.run({
      eachMessage: async ({ message }) => {
        const headers: Record<string, string> = {};
        for (const [k, v] of Object.entries(message.headers ?? {})) if (v !== undefined) headers[k] = v.toString();
        out.push({ key: message.key?.toString() ?? "", headers, value: message.value ?? Buffer.alloc(0) });
      },
    });
    for (let i = 0; i < 400 && out.length < n; i++) await sleep(50);
    await c.disconnect();
    return out;
  }

  test("mapper: topic, key, group and dead-letter topic", async () => {
    const ch = channel(`map.v1.M${run}`, `map.m${run}.{id}`);
    const topic = `map-${run}`;
    const b = new KafkaBackend(brokers, {
      partitions: 1,
      topics: topicTable({
        [ch.key]: { name: topic, key: (subject) => `K:${subject}`, group: (g) => `grp-${run}-${g}`, dlq: `dead-${run}` },
      }),
    });
    const core = new Core(b, { logf: () => {} });
    try {
      let seen: Message | undefined;
      const stop = await core.subscribe(
        ch,
        "g",
        (_s, m) => {
          if (m.headers["Fail"] === "1") {
            seen = m;
            throw new Error("boom");
          }
        },
        { maxDeliver: 1 },
      );
      await core.publish(ch, ["a"], "t.Body", enc.encode("hi"), { msgId: "id-1", headers: { Fail: "0" } });
      await core.publish(ch, ["b"], "t.Body", enc.encode("bad"), { msgId: "id-2", headers: { Fail: "1" } });
      const recs = await readAll(topic, 2);
      assert.deepEqual(recs.map((r) => r.key).sort(), [`K:map.m${run}.a`, `K:map.m${run}.b`]);
      const a = recs.find((r) => r.key.endsWith(".a"))!;
      assert.equal(a.headers["Gwag-Subject"], `map.m${run}.a`);
      assert.equal(a.headers["Gwag-Msg-Id"], "id-1");
      assert.equal(a.headers["Gwag-Type"], "t.Body");
      assert.equal(a.headers["Gwag-Channel"], ch.key);
      const dead = await readAll(`dead-${run}`, 1);
      assert.equal(dead[0]!.headers["Gwag-Error"], "boom");
      assert.equal(dead[0]!.headers["Gwag-Subject"], `map.m${run}.b`);
      assert.equal(dead[0]!.value.toString(), "bad");
      assert.ok(seen);
      assert.equal(seen.subject, `map.m${run}.b`);
      assert.equal(seen.key && Buffer.from(seen.key).toString(), `K:map.m${run}.b`);
      const admin = kafka.admin();
      await admin.connect();
      const groups = (await admin.listGroups()).groups.map((g) => g.groupId);
      await admin.disconnect();
      assert.ok(groups.includes(`grp-${run}-g`), `groups ${groups}`);
      await stop();
    } finally {
      await b.close();
    }
  });

  test("retryTopic: the partition moves on; delivery counts up; the DLQ after maxDeliver", async () => {
    const ch = channel(`rt.v1.R${run}`, `rt.r${run}.{id}`);
    const b = new KafkaBackend(brokers, { partitions: 1 });
    const core = new Core(b, { logf: () => {} });
    try {
      const attempts: Record<string, number[]> = {};
      const stop = await core.subscribe(
        ch,
        "g",
        (_s, m) => {
          (attempts[m.id] ??= []).push(m.delivery);
          assert.equal(m.headers["Gwag-Attempt"], undefined);
          assert.equal(m.headers["Gwag-Retry-At"], undefined);
          if (m.id === "flaky" && m.delivery < 3) throw new Error("not yet");
          if (m.id === "dead") throw new Error("always");
        },
        { ...retryTopic(), maxDeliver: 3, backoffMs: [100, 100] },
      );
      await core.publish(ch, ["x"], "t.Body", enc.encode("1"), { msgId: "flaky" });
      await core.publish(ch, ["x"], "t.Body", enc.encode("2"), { msgId: "dead" });
      await core.publish(ch, ["x"], "t.Body", enc.encode("3"), { msgId: "fine" });
      for (let i = 0; i < 400 && !(attempts["flaky"]?.includes(3) && attempts["dead"]?.length === 3); i++) await sleep(50);
      assert.deepEqual(attempts["flaky"], [1, 2, 3]);
      assert.deepEqual(attempts["dead"], [1, 2, 3]);
      assert.deepEqual(attempts["fine"], [1]);
      const dead = await readAll(`gwag.dlq.${ch.stream}`, 1);
      assert.equal(dead[0]!.headers["Gwag-Error"], "always");
      assert.equal(dead[0]!.headers["Gwag-Attempt"], undefined);
      const retries = await readAll(`gwag.retry.${ch.stream}.g`, 4);
      assert.equal(retries.length, 4, "flaky x2 and dead x2");
      assert.ok(retries.every((r) => r.headers["Gwag-Attempt"] && r.headers["Gwag-Retry-At"]));
      await stop();
    } finally {
      await b.close();
    }
  });

  test("records without gwag headers: subject from the pattern or the key", async () => {
    const lit = channel(`foreign.v1.L${run}`, `foreign-lit-${run}`);
    const wild = channel(`foreign.v1.W${run}`, `events.foreign.{id}.w${run}`);
    const b = new KafkaBackend(brokers, { partitions: 1 });
    const core = new Core(b, { logf: () => {} });
    try {
      const got: string[] = [];
      const stops = [
        await core.subscribe(lit, "g", (_s, m) => void got.push(`lit ${m.subject} ${Buffer.from(m.key!).toString()}`)),
        await core.subscribe(wild, "g", (s, m) => void got.push(`wild ${m.subject} ${s[0]}`)),
      ];
      const producer = kafka.producer();
      await producer.connect();
      await producer.send({ topic: lit.stream, messages: [{ key: "k1", value: Buffer.from("x") }] });
      await producer.send({ topic: wild.stream, messages: [{ key: `events.foreign.o-1.w${run}`, value: Buffer.from("y") }] });
      await producer.disconnect();
      for (let i = 0; i < 200 && got.length < 2; i++) await sleep(50);
      assert.deepEqual(got.sort(), [`lit foreign-lit-${run} k1`, `wild events.foreign.o-1.w${run} o-1`]);
      for (const s of stops) await s();
    } finally {
      await b.close();
    }
  });
}
