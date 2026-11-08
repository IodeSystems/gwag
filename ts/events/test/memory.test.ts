import assert from "node:assert/strict";
import { test } from "node:test";
import { MemoryBackend } from "../src/memory.ts";
import { runBackendSuite } from "../src/eventstest.ts";

runBackendSuite("memory", {
  newBackend: () => new MemoryBackend(),
  deadLetters: async (b, _stream, n) => {
    const mem = b as MemoryBackend;
    for (let i = 0; i < 400 && mem.deadLetters().length < n; i++) await new Promise((r) => setTimeout(r, 5));
    assert.ok(mem.deadLetters().length >= n, "dead letters");
    return mem.deadLetters();
  },
});

test("memory close ends subscribers", async () => {
  const b = new MemoryBackend();
  await b.close();
  await assert.rejects(b.publish({ key: "a", stream: "EV_a" } as never, { subject: "a", id: "", headers: {}, data: new Uint8Array(), delivery: 0 }));
});
