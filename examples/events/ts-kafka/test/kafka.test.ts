// Needs Kafka: KAFKA_BROKERS=127.0.0.1:19092. ../test.sh starts
// apache/kafka in docker and sets it. Without it the suite is skipped,
// and says so.
import assert from "node:assert/strict";
import { test } from "node:test";
import { channel, dlqSubject, HeaderMsgID, resolveSubscribeOptions, type Message } from "@gwag/events";
import { runBackendSuite } from "@gwag/events/eventstest";
import { Kafka, logLevel } from "kafkajs";
import { KafkaBackend } from "../src/kafka.ts";

const brokers = process.env.KAFKA_BROKERS?.split(",");

if (!brokers) {
  test("kafka backend", { skip: "KAFKA_BROKERS not set; run ../test.sh" }, () => {});
} else {
  runBackendSuite("kafka", {
    newBackend: () => new KafkaBackend(brokers),
    deadLetters: async (_b, stream, n) => {
      const kafka = new Kafka({ brokers, logLevel: logLevel.NOTHING });
      // The dead-letter topic appears at the first dead letter.
      const admin = kafka.admin();
      await admin.connect();
      for (let i = 0; i < 300; i++) {
        if ((await admin.listTopics()).includes(dlqSubject(stream))) break;
        await new Promise((r) => setTimeout(r, 100));
      }
      await admin.disconnect();
      const consumer = kafka.consumer({ groupId: `dlq-reader-${Date.now()}-${Math.random()}` });
      await consumer.connect();
      await consumer.subscribe({ topic: dlqSubject(stream), fromBeginning: true });
      const out: Message[] = [];
      await consumer.run({
        eachMessage: async ({ topic, message }) => {
          const headers: Record<string, string> = {};
          for (const [k, v] of Object.entries(message.headers ?? {})) if (v !== undefined) headers[k] = v.toString();
          out.push({ subject: topic, id: headers[HeaderMsgID] ?? "", headers, data: new Uint8Array(message.value ?? []), delivery: 0 });
        },
      });
      for (let i = 0; i < 600 && out.length < n; i++) await new Promise((r) => setTimeout(r, 50));
      await consumer.disconnect();
      assert.ok(out.length >= n, `got ${out.length} dead letters, want ${n}`);
      return out;
    },
    noDedup: true,
  });

  // A record published right after subscribe returns must reach a new
  // group. kafkajs resolves a new group's start at its first fetch,
  // after the join, unless the backend seeds it.
  test("publish right after subscribe is delivered", async () => {
    const run = Date.now().toString(36);
    // Trials run in parallel: each stop waits out a fetch long-poll.
    const trial = async (i: number): Promise<boolean> => {
      const b = new KafkaBackend(brokers);
      try {
        const ch = channel(`race.v1.C${run}_${i}`, `race.c${run}_${i}.{id}`);
        let got = 0;
        const stop = await b.subscribe(ch, "g", resolveSubscribeOptions(), async () => {
          got++;
        });
        await b.publish(ch, { subject: `race.c${run}_${i}.x`, id: `m${i}`, headers: {}, data: new Uint8Array([1]), delivery: 0 });
        for (let t = 0; t < 400 && got === 0; t++) await new Promise((r) => setTimeout(r, 25));
        await stop();
        return got > 0;
      } finally {
        await b.close();
      }
    };
    const ok = await Promise.all(Array.from({ length: 8 }, (_, i) => trial(i)));
    const lost = ok.flatMap((v, i) => (v ? [] : [i]));
    assert.deepEqual(lost, [], `lost on trials ${lost}`);
  });
}
