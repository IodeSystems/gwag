// Needs Kafka: KAFKA_BROKERS=127.0.0.1:19092. ../test.sh starts
// apache/kafka in docker and sets it. Without it the suite is skipped,
// and says so.
import assert from "node:assert/strict";
import { test } from "node:test";
import { dlqSubject, HeaderMsgID, type Message } from "@gwag/events";
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
    // Offsets of a new group resolve after assignment.
    settleMs: 1000,
    noDedup: true,
  });
}
