// Needs a NATS server with JetStream: NATS_URL=nats://127.0.0.1:4222.
// ts/events/test.sh starts one in docker. Without NATS_URL these tests
// are skipped, and say so.
import assert from "node:assert/strict";
import { after, describe, test } from "node:test";
import { jetstreamManager } from "@nats-io/jetstream";
import { connect, nanos, type NatsConnection } from "@nats-io/transport-node";
import { channel, Core, dlqSubject, newID, type Backend, type Message } from "../src/index.ts";
import { runBackendSuite, testChannel } from "../src/eventstest.ts";
import { JetStreamBackend, streamConfig } from "../src/jetstream.ts";

const url = process.env.NATS_URL;
const skip = url ? false : "NATS_URL not set (run ts/events/test.sh)";

const conns: NatsConnection[] = [];
async function conn(): Promise<NatsConnection> {
  const nc = await connect({ servers: url! });
  conns.push(nc);
  return nc;
}
after(async () => {
  for (const nc of conns) await nc.close();
});

class WithDLQ extends JetStreamBackend {
  readonly dead: Message[] = [];
  constructor(nc: NatsConnection) {
    // The test listener on gwag.dlq.> receives the publish but never acks it,
    // so the backend logs a publish timeout; same as the Go backend.
    super(nc, { prefetch: 1, logf: () => {} });
    const sub = nc.subscribe("gwag.dlq.>");
    void (async () => {
      for await (const m of sub) {
        const headers: Record<string, string> = {};
        for (const k of m.headers?.keys() ?? []) headers[k] = m.headers!.values(k)[0]!;
        this.dead.push({ subject: m.subject, id: "", headers, data: m.data, delivery: 0 });
      }
    })();
  }
}

if (skip) {
  test("jetstream backend", { skip }, () => {});
} else {
  runBackendSuite("jetstream", {
    newBackend: async () => new WithDLQ(await conn()),
    deadLetters: async (b, stream, n) => {
      const w = b as WithDLQ;
      const want = dlqSubject(stream);
      for (let i = 0; i < 2000; i++) {
        const got = w.dead.filter((m) => m.subject === want);
        if (got.length >= n) return got;
        await new Promise((r) => setTimeout(r, 5));
      }
      assert.fail(`no ${n} dead letters`);
    },
    settleMs: 20,
  });
}

describe("jetstream specifics", { skip }, () => {
  test("stream is created only if missing", async () => {
    const nc = await conn();
    const jsm = await jetstreamManager(nc);
    const core = new Core(new JetStreamBackend(nc), { logf: () => {} });
    const key = `only.v1.E${newID().slice(0, 8)}`;

    const existing = channel(key, `only.e${key.slice(-8)}.{id}`);
    await jsm.streams.add({ ...streamConfig(existing), max_age: nanos(60 * 60 * 1000) });
    await core.publish(existing, ["o1"], "t", new Uint8Array([1]));
    assert.equal((await jsm.streams.info(existing.stream)).config.max_age, nanos(60 * 60 * 1000));

    const fresh = testChannel();
    await core.publish(fresh, ["x"], "t", new Uint8Array([1]));
    const cfg = (await jsm.streams.info(fresh.stream)).config;
    assert.equal(cfg.max_age, nanos(7 * 24 * 60 * 60 * 1000));
    assert.equal(cfg.duplicate_window, nanos(2 * 60 * 1000));
    assert.deepEqual(cfg.subjects, [fresh.pattern.wildcard()]);
    assert.equal(cfg.storage, "file");
    assert.equal(cfg.retention, "limits");
    assert.equal(cfg.num_replicas, 1);
  });

  test("group is one durable consumer", async () => {
    const nc = await conn();
    const b: Backend = new JetStreamBackend(nc);
    const core = new Core(b, { logf: () => {} });
    const ch = testChannel();
    const got: string[] = [];
    const stop = await core.subscribe(ch, "g", () => void got.push("first"));
    await stop();
    await new Promise((r) => setTimeout(r, 50));
    await core.publish(ch, ["k"], "t", new TextEncoder().encode("while away"));
    const stop2 = await core.subscribe(ch, "g", () => void got.push("back"));
    for (let i = 0; i < 1000 && got.length < 1; i++) await new Promise((r) => setTimeout(r, 5));
    await stop2();
    assert.deepEqual(got, ["back"]);
    const jsm = await jetstreamManager(nc);
    const ci = await jsm.consumers.info(ch.stream, "g");
    assert.equal(ci.config.ack_policy, "explicit");
    assert.equal(ci.config.deliver_policy, "new");
    assert.equal(ci.config.max_deliver, 5);
    assert.equal(ci.config.ack_wait, nanos(30000));
  });
});
