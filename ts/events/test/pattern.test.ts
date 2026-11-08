import assert from "node:assert/strict";
import { test } from "node:test";
import { canonicalHeaderKey, parsePattern, streamName } from "../src/index.ts";

test("parse, fill, match", () => {
  const p = parsePattern("events.orders.{orderId}.*.>");
  assert.deepEqual(p.segments(), [
    { name: "orderId", kind: "named" },
    { name: "P1", kind: "positional" },
    { name: "Rest", kind: "rest" },
  ]);
  assert.equal(p.wildcard(), "events.orders.*.*.>");
  const s = p.subject(["o1", "eu", "a.b"]);
  assert.equal(s, "events.orders.o1.eu.a.b");
  assert.deepEqual(p.match(s), ["o1", "eu", "a.b"]);
});

test("bad patterns", () => {
  for (const p of ["", "a..b", "a.>.b", "a.{1x}", "a.{x}.{x}", "a.b*", "a.{x"]) {
    assert.throws(() => parsePattern(p));
  }
});

test("subject rejects bad values", () => {
  const p = parsePattern("a.{x}");
  for (const v of ["", "b.c", "*", ">", "a b"]) assert.throws(() => p.subject([v]));
  assert.throws(() => p.subject([]));
});

test("match", () => {
  const p = parsePattern("a.*.c");
  const want: Record<string, boolean> = { "a.b.c": true, "a.b": false, "a.b.c.d": false, "x.b.c": false };
  for (const [subj, ok] of Object.entries(want)) assert.equal(p.match(subj) !== undefined, ok, subj);
  const q = parsePattern("a.>");
  assert.equal(q.match("a"), undefined);
  assert.deepEqual(q.match("a.b.c"), ["b.c"]);
});

test("names", () => {
  assert.equal(streamName("orders.v1.OrderEvents"), "EV_orders_v1_OrderEvents");
  assert.equal(canonicalHeaderKey("gwag-type"), "Gwag-Type");
  assert.equal(canonicalHeaderKey("X-TRACE"), "X-Trace");
});
