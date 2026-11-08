import assert from "node:assert/strict";
import { test } from "node:test";
import { channel, Core, HeaderType, type Message, type Serde } from "../src/index.ts";
import { MemoryBackend } from "../src/memory.ts";

// A serde that prefixes the body with the topic and strips it again.
class Prefix implements Serde {
  topics: string[] = [];
  encode(_ch: unknown, topic: string, type: unknown, out: Message): void {
    this.topics.push(topic);
    assert.equal(typeof type, "string");
    out.data = new Uint8Array([...new TextEncoder().encode(topic + "|"), ...out.data]);
  }
  decode(_ch: unknown, topic: string, m: Message): void {
    const p = new TextEncoder().encode(topic + "|");
    assert.deepEqual([...m.data.subarray(0, p.length)], [...p]);
    m.data = m.data.subarray(p.length);
    m.headers[HeaderType] ??= "from.serde";
  }
}

class Named extends MemoryBackend {
  topic(): string {
    return "my-topic";
  }
}

test("serde: encode and decode wrap the body; the topic comes from the backend", async () => {
  const mem = new Named();
  const serde = new Prefix();
  const core = new Core(mem, { serde });
  const ch = channel("t.v1.C", "t.c.{id}");
  const got: Message[] = [];
  await core.subscribe(ch, "g", (_s, m) => void got.push(m), { deliverAll: true });
  await core.publish(ch, ["x"], "t.Body", new Uint8Array([1, 2]));
  for (let i = 0; i < 200 && got.length === 0; i++) await new Promise((r) => setTimeout(r, 5));
  assert.deepEqual(serde.topics, ["my-topic"]);
  assert.deepEqual([...got[0]!.data], [1, 2]);
  assert.equal(got[0]!.headers[HeaderType], "t.Body");
  await mem.close();
});

test("serde: a decode error fails the delivery", async () => {
  const mem = new MemoryBackend();
  const serde: Serde = {
    encode() {},
    decode() {
      throw new Error("no schema");
    },
  };
  const core = new Core(mem, { serde });
  const ch = channel("t.v1.D", "t.d.{id}");
  let handled = 0;
  await core.subscribe(ch, "g", () => void handled++, { maxDeliver: 1, backoffMs: [] });
  await core.publish(ch, ["x"], "t.Body", new Uint8Array([1]));
  for (let i = 0; i < 200 && mem.deadLetters().length === 0; i++) await new Promise((r) => setTimeout(r, 5));
  assert.equal(handled, 0);
  assert.equal(mem.deadLetters().length, 1);
  assert.match(mem.deadLetters()[0]!.headers["Gwag-Error"] ?? "", /decode t.v1.D: no schema/);
  await mem.close();
});
