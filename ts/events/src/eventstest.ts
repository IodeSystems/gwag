// Shared behaviour checks every Backend must pass, the TypeScript
// counterpart of events/eventstest: fan-out across groups, work
// sharing in a group, redelivery, dead-lettering, dedup, deliverAll.
// Backend packages call runBackendSuite from their tests (node:test).
import assert from "node:assert/strict";
import { describe, it } from "node:test";
import {
  channel,
  Core,
  dlqSubject,
  HeaderChannel,
  HeaderError,
  messageType,
  newID,
  type Backend,
  type Channel,
  type Message,
  type SegmentHandler,
  type Stop,
  type SubscribeOptions,
} from "./index.ts";

export interface Harness {
  /** An empty backend. Calls must not see each other's messages (channel keys are unique anyway). */
  newBackend: () => Promise<Backend> | Backend;
  /** Waits until n dead letters for stream exist and returns them. */
  deadLetters: (b: Backend, stream: string, n: number) => Promise<Message[]>;
  /** Wait after subscribe returns until the subscriber is receiving. */
  settleMs?: number;
  /** Skip the dedup check, for backends without message-id dedup (Kafka). */
  noDedup?: boolean;
}

const run = newID().slice(0, 8);
let seq = 0;

/** A channel with a key unique to this process. */
export function testChannel(): Channel {
  seq++;
  return channel(`test.v1.C${run}_${seq}`, `test.c${run}_${seq}.{id}`);
}

const enc = new TextEncoder();
const dec = new TextDecoder();
const sleep = (ms: number) => new Promise<void>((r) => setTimeout(r, ms));

async function waitFor(what: string, cond: () => boolean, timeoutMs = 10000): Promise<void> {
  const end = Date.now() + timeoutMs;
  while (!cond()) {
    if (Date.now() > end) assert.fail(`timed out waiting for ${what}`);
    await sleep(5);
  }
}

export function runBackendSuite(name: string, h: Harness): void {
  describe(`backend suite: ${name}`, () => {
    const stops: Stop[] = [];
    const backends: Backend[] = [];

    async function setup() {
      const b = await h.newBackend();
      backends.push(b);
      return { core: new Core(b, { logf: () => {} }), b, ch: testChannel() };
    }
    async function sub(core: Core, ch: Channel, group: string, fn: SegmentHandler, opts?: SubscribeOptions) {
      stops.push(await core.subscribe(ch, group, fn, opts));
      await sleep(h.settleMs ?? 0);
    }
    const pub = (core: Core, ch: Channel, id: string, body: string, o?: { msgId?: string; headers?: Record<string, string> }) =>
      core.publish(ch, [id], "test.Body", enc.encode(body), o);

    // after() hooks run per describe; do cleanup after each test.
    async function cleanup() {
      for (const s of stops.splice(0)) await s();
      for (const b of backends.splice(0)) await b.close();
    }

    it("fans out across groups", async () => {
      const { core, ch } = await setup();
      try {
        const a: string[] = [];
        const b: string[] = [];
        await sub(core, ch, "a", (segs, m) => {
          assert.equal(messageType(m), "test.Body");
          assert.equal(m.headers[HeaderChannel], ch.key);
          a.push(`${segs[0]}=${dec.decode(m.data)}`);
        });
        await sub(core, ch, "b", (segs, m) => {
          b.push(`${segs[0]}=${dec.decode(m.data)}`);
        });
        await pub(core, ch, "x1", "hello");
        await waitFor("both groups", () => a.length === 1 && b.length === 1);
        assert.deepEqual(a, ["x1=hello"]);
        assert.deepEqual(b, ["x1=hello"]);
        assert.deepEqual(core.subscriptions(), [
          { channel: ch.key, group: "a" },
          { channel: ch.key, group: "b" },
        ]);
      } finally {
        await cleanup();
      }
    });

    it("shares work inside a group", async () => {
      const { core, ch } = await setup();
      try {
        const one: string[] = [];
        const two: string[] = [];
        const slow = (into: string[]): SegmentHandler => async (_s, m) => {
          await sleep(5);
          into.push(dec.decode(m.data));
        };
        await sub(core, ch, "shared", slow(one));
        await sub(core, ch, "shared", slow(two));
        const n = 40;
        for (let i = 0; i < n; i++) await pub(core, ch, `k${i}`, String(i));
        await waitFor("all messages", () => one.length + two.length >= n);
        await sleep(50);
        assert.equal(one.length + two.length, n, "each message once per group");
        assert.ok(one.length > 0 && two.length > 0, `work not shared: ${one.length} / ${two.length}`);
        assert.equal(core.subscriptions().length, 1);
      } finally {
        await cleanup();
      }
    });

    it("redelivers after an error", async () => {
      const { core, ch } = await setup();
      try {
        const deliveries: number[] = [];
        await sub(
          core,
          ch,
          "r",
          (_s, m) => {
            deliveries.push(m.delivery);
            if (m.delivery < 3) throw new Error("not yet");
          },
          { backoffMs: [10], maxDeliver: 5 },
        );
        await pub(core, ch, "k", "retry me");
        await waitFor("third delivery", () => deliveries.length >= 3);
        await sleep(50);
        assert.deepEqual(deliveries, [1, 2, 3]);
      } finally {
        await cleanup();
      }
    });

    it("dead-letters after maxDeliver", async () => {
      const { core, ch, b } = await setup();
      try {
        let calls = 0;
        await sub(
          core,
          ch,
          "d",
          () => {
            calls++;
            throw new Error("boom");
          },
          { backoffMs: [5], maxDeliver: 2 },
        );
        await pub(core, ch, "k", "doomed", { headers: { "X-Trace": "t1" } });
        const dead = await h.deadLetters(b, ch.stream, 1);
        const m = dead[0]!;
        assert.equal(m.subject, dlqSubject(ch.stream));
        assert.equal(m.headers[HeaderError], "boom");
        assert.equal(m.headers["X-Trace"], "t1");
        assert.equal(messageType(m), "test.Body");
        assert.equal(dec.decode(m.data), "doomed");
        await sleep(50);
        assert.equal(calls, 2);
      } finally {
        await cleanup();
      }
    });

    it("dedups a repeated message id", { skip: h.noDedup }, async () => {
      const { core, ch } = await setup();
      try {
        const got: string[] = [];
        await sub(core, ch, "u", (_s, m) => {
          got.push(`${dec.decode(m.data)}/${m.id}`);
        });
        await pub(core, ch, "k", "first", { msgId: "same" });
        await pub(core, ch, "k", "second", { msgId: "same" });
        await pub(core, ch, "k", "third");
        await waitFor("two messages", () => got.length >= 2);
        await sleep(50);
        assert.equal(got.length, 2);
        assert.equal(got[0], "first/same");
      } finally {
        await cleanup();
      }
    });

    it("starts a deliverAll group at the first stored message", async () => {
      const { core, ch } = await setup();
      try {
        await pub(core, ch, "k", "old");
        const fresh: string[] = [];
        const all: string[] = [];
        await sub(core, ch, "new", (_s, m) => void fresh.push(dec.decode(m.data)));
        await sub(core, ch, "all", (_s, m) => void all.push(dec.decode(m.data)), { deliverAll: true });
        await pub(core, ch, "k", "new");
        await waitFor("deliveries", () => fresh.length >= 1 && all.length >= 2);
        await sleep(50);
        assert.deepEqual(fresh, ["new"]);
        assert.deepEqual(all, ["old", "new"]);
      } finally {
        await cleanup();
      }
    });
  });
}
