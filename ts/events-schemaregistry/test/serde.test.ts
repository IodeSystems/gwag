// Serde tests against SR_URL (a real registry; ../test.sh starts one)
// or an in-memory fake registry.
import assert from "node:assert/strict";
import { createServer, type Server } from "node:http";
import { after, test } from "node:test";
import { create, toBinary, fromBinary } from "@bufbuild/protobuf";
import { channel, Core, newID, type Message } from "@gwag/events";
import { MemoryBackend } from "@gwag/events/memory";
import {
  OrderCancelledSchema,
  OrderCreatedSchema,
  OrderShippedSchema,
} from "../../../examples/events/ts/src/gen/orders/v1/orders_pb.ts";
import {
  frame,
  HeaderSchemaID,
  isNotFound,
  messageIndexes,
  NotFramedError,
  NotRegisteredError,
  RegistryClient,
  SchemaRegistrySerde,
  shapeOfText,
  strategyByName,
  topicRecordNameStrategy,
  unframe,
  type Schema,
} from "../src/index.ts";

interface Stored extends Schema {
  id: number;
  subject: string;
  version: number;
}

// A fake registry: just enough of the REST API for the serde.
async function fakeRegistry(): Promise<{ url: string; server: Server; schemas: Stored[]; seen: string[] }> {
  const schemas: Stored[] = [];
  const seen: string[] = [];
  const server = createServer((req, res) => {
    const chunks: Buffer[] = [];
    req.on("data", (c: Buffer) => chunks.push(c));
    req.on("end", () => {
      seen.push(`${req.method} ${req.url} ${req.headers["authorization"] ?? ""}`);
      const url = new URL(req.url ?? "/", "http://x");
      const body = chunks.length > 0 ? (JSON.parse(Buffer.concat(chunks).toString()) as Schema) : undefined;
      const send = (status: number, v: unknown) => {
        res.writeHead(status, { "Content-Type": "application/json" });
        res.end(JSON.stringify(v));
      };
      let m: RegExpMatchArray | null;
      if ((m = url.pathname.match(/^\/subjects\/([^/]+)\/versions$/)) && req.method === "POST" && body) {
        const subject = decodeURIComponent(m[1]!);
        let s = schemas.find((x) => x.subject === subject && x.schema === body.schema);
        if (!s) {
          const known = schemas.find((x) => x.schema === body.schema);
          s = {
            ...body,
            id: known?.id ?? schemas.length + 100,
            subject,
            version: schemas.filter((x) => x.subject === subject).length + 1,
          };
          schemas.push(s);
        }
        return send(200, { id: s.id });
      }
      if ((m = url.pathname.match(/^\/subjects\/([^/]+)$/)) && req.method === "POST" && body) {
        const subject = decodeURIComponent(m[1]!);
        const s = schemas.find((x) => x.subject === subject && x.schema === body.schema);
        if (!s) return send(404, { error_code: 40401, message: `Subject '${subject}' not found.` });
        return send(200, s);
      }
      if ((m = url.pathname.match(/^\/schemas\/ids\/(\d+)$/))) {
        const s = schemas.find((x) => x.id === Number(m![1]));
        if (!s) return send(404, { error_code: 40403, message: "Schema not found" });
        return send(200, { schema: s.schema, schemaType: "PROTOBUF", references: s.references });
      }
      if ((m = url.pathname.match(/^\/subjects\/([^/]+)\/versions\/(.+)$/))) {
        const subject = decodeURIComponent(m[1]!);
        const all = schemas.filter((x) => x.subject === subject);
        const s = m[2] === "latest" ? all[all.length - 1] : all.find((x) => String(x.version) === m![2]);
        if (!s) return send(404, { error_code: 40402, message: "Version not found" });
        return send(200, s);
      }
      if (url.pathname.startsWith("/config/")) return send(200, { compatibilityLevel: "FULL" });
      send(404, { error_code: 404, message: "no route" });
    });
  });
  await new Promise<void>((r) => server.listen(0, "127.0.0.1", r));
  const addr = server.address();
  const port = typeof addr === "object" && addr ? addr.port : 0;
  return { url: `http://127.0.0.1:${port}`, server, schemas, seen };
}

const fake = process.env.SR_URL ? undefined : await fakeRegistry();
after(() => fake?.server.close());
const registry = () => new RegistryClient({ url: process.env.SR_URL ?? fake!.url });

const orderEvents = channel("orders.v1.OrderEvents", "events.orders.{orderId}.lifecycle");

test("frame: same bytes as events/schemaregistry TestFrame", () => {
  const b = new Uint8Array([98]);
  const cases: [number[], number[]][] = [
    [[0], [0, 0, 0, 1, 2, 0, 98]],
    [[1], [0, 0, 0, 1, 2, 2, 2, 98]],
    [[2, 0], [0, 0, 0, 1, 2, 4, 4, 0, 98]],
  ];
  for (const [idx, want] of cases) {
    const got = frame(258, idx, b);
    assert.deepEqual([...got], want, `frame ${idx}`);
    const u = unframe(got);
    assert.equal(u.id, 258);
    assert.deepEqual(u.indexes, idx);
    assert.deepEqual([...u.body], [98]);
  }
  assert.throws(() => unframe(new Uint8Array([1, 2, 3, 4, 5, 6])), NotFramedError);
  assert.deepEqual(messageIndexes(OrderCreatedSchema), [4]);
  // Large ids and index counts survive.
  assert.equal(unframe(frame(0xfffffffe, [300, 5], b)).id, 0xfffffffe);
  assert.deepEqual(unframe(frame(1, [300, 5], b)).indexes, [300, 5]);
});

test("own: autoRegister registers the file and its options import; the subscriber decodes", async () => {
  const c = registry();
  const topic = "own-" + newID().slice(0, 8);
  const mem = new MemoryBackend();
  const backend = Object.assign(mem, { topic: () => topic });
  const core = new Core(backend, { serde: new SchemaRegistrySerde(c, { autoRegister: true }), logf: () => {} });
  const got: Message[] = [];
  await core.subscribe(orderEvents, "g", (_s, m) => void got.push(m));
  await core.publish(orderEvents, ["o-1"], OrderCancelledSchema, toBinary(OrderCancelledSchema, create(OrderCancelledSchema, { orderId: "o-1", reason: "r" })));
  for (let i = 0; i < 200 && got.length === 0; i++) await new Promise((r) => setTimeout(r, 5));
  const m = got[0]!;
  assert.equal(fromBinary(OrderCancelledSchema, m.data).reason, "r");
  assert.equal(m.headers["Gwag-Type"], "orders.v1.OrderCancelled");
  assert.ok(m.headers[HeaderSchemaID]);
  const v = await c.subjectVersion(`${topic}-value`, "latest");
  assert.deepEqual(v.references?.map((r) => [r.name, r.subject]), [["gw/proto/ps/v1/options.proto", "gw/proto/ps/v1/options.proto"]]);
  await mem.close();
});

test("a framed record without gwag headers gets its type from the schema id; typeNames renames", async () => {
  const c = registry();
  const topic = "imp-" + newID().slice(0, 8);
  const body = toBinary(OrderCreatedSchema, create(OrderCreatedSchema, { orderId: "o-9", customer: "bo" }));
  const out: Message = { subject: "s", id: "", headers: {}, data: body, delivery: 0 };
  await new SchemaRegistrySerde(c, { autoRegister: true }).encode(orderEvents, topic, OrderCreatedSchema, out);
  assert.equal(out.data[0], 0);
  const mk = (): Message => ({ subject: "s", id: "", headers: {}, data: out.data, delivery: 0 });
  let m = mk();
  await new SchemaRegistrySerde(c).decode(orderEvents, topic, m);
  assert.equal(m.headers["Gwag-Type"], "orders.v1.OrderCreated");
  assert.equal(fromBinary(OrderCreatedSchema, m.data).customer, "bo");
  m = mk();
  await new SchemaRegistrySerde(c, { typeNames: { "orders.v1.OrderCreated": "shop.v1.Created" } }).decode(orderEvents, topic, m);
  assert.equal(m.headers["Gwag-Type"], "shop.v1.Created");
  // A header that disagrees with the schema fails the delivery.
  m = mk();
  m.headers["Gwag-Type"] = "orders.v1.Other";
  await assert.rejects(new SchemaRegistrySerde(c).decode(orderEvents, topic, m), /frames orders.v1.OrderCreated/);
  // Plain proto passes through.
  m = { subject: "s", id: "", headers: { "Gwag-Type": "orders.v1.OrderCreated" }, data: body, delivery: 0 };
  await new SchemaRegistrySerde(c).decode(orderEvents, topic, m);
  assert.deepEqual([...m.data], [...body]);
});

test("lookup-only fails until the schema is registered; useLatest and useSchemaId frame the same message", async () => {
  const c = registry();
  const topic = "look-" + newID().slice(0, 8);
  const body = toBinary(OrderCreatedSchema, create(OrderCreatedSchema, {}));
  const mk = (): Message => ({ subject: "s", id: "", headers: {}, data: body, delivery: 0 });
  await assert.rejects(new SchemaRegistrySerde(c).encode(orderEvents, topic, OrderCreatedSchema, mk()), NotRegisteredError);
  const reg = new SchemaRegistrySerde(c, { autoRegister: true });
  const first = mk();
  await reg.encode(orderEvents, topic, OrderCreatedSchema, first);
  const id = unframe(first.data).id;
  for (const s of [new SchemaRegistrySerde(c), new SchemaRegistrySerde(c, { useLatest: true }), new SchemaRegistrySerde(c, { useSchemaId: id })]) {
    const m = mk();
    await s.encode(orderEvents, topic, OrderCreatedSchema, m);
    const u = unframe(m.data);
    assert.equal(u.id, id);
    assert.deepEqual(u.indexes, [4]);
  }
  await assert.rejects(new SchemaRegistrySerde(c).encode(orderEvents, topic, "orders.v1.OrderCreated", mk()), /needs the message schema/);
  const s = new SchemaRegistrySerde(c, { subjectNameStrategy: topicRecordNameStrategy });
  assert.equal(s.subject("t", OrderCreatedSchema), "t-orders.v1.OrderCreated");
  assert.equal(strategyByName("record")(topic, OrderShippedSchema), "orders.v1.OrderShipped");
  assert.throws(() => strategyByName("nope"), /want topic, record or topic-record/);
});

test("shapeOfText finds the package and message nesting", () => {
  const shape = shapeOfText(`
    syntax = "proto3"; // message Fake {
    package corp.orders.v1;
    import "x.proto";
    enum Status { A = 0; }
    /* message Nope { */
    message Order {
      option (a) = { b: 1 };
      message Line { string sku = 1; }
      oneof k { string a = 1; }
      Line line = 2;
    }
    message Second { message In { message Deep {} } }
    service S { rpc M (Order) returns (Second); }
  `);
  assert.equal(shape.package, "corp.orders.v1");
  assert.deepEqual(shape.messages.map((m) => m.name), ["Order", "Second"]);
  assert.deepEqual(shape.messages[0]!.messages.map((m) => m.name), ["Line"]);
  assert.equal(shape.messages[1]!.messages[0]!.messages[0]!.name, "Deep");
});

test("client: basic auth from the URL, bearer token, not-found errors", async () => {
  const f = await fakeRegistry();
  try {
    const u = new URL(f.url);
    const basic = new RegistryClient({ url: `http://u:p@${u.host}` });
    assert.equal(basic.url, f.url);
    assert.equal(await basic.compatibilityLevel("s"), "FULL");
    const bearer = new RegistryClient({ url: f.url, token: "tok" });
    await bearer.compatibilityLevel("s");
    assert.deepEqual(f.seen.map((s) => s.split(" ")[2] + " " + s.split(" ")[3]), ["Basic dTpw", "Bearer tok"]);
    await assert.rejects(basic.subjectVersion("missing", "latest"), (e) => isNotFound(e));
    assert.throws(() => new RegistryClient({ url: "ftp://x" }), /want http or https/);
  } finally {
    f.server.close();
  }
});

test("autoRegister, useLatest and useSchemaId are mutually exclusive", () => {
  const c = registry();
  const msg = "schema registry: auto-register, use-latest and use-schema-id are mutually exclusive; set at most one";
  for (const o of [
    { autoRegister: true, useLatest: true },
    { useSchemaId: 3, autoRegister: true },
    { useLatest: true, useSchemaId: 3 },
  ]) {
    assert.throws(() => new SchemaRegistrySerde(c, o), { message: msg });
  }
  new SchemaRegistrySerde(c, { useLatest: true, autoRegister: false });
});
