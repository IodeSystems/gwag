// billing: a TypeScript service that calls orders through the gateway.
// It registers with gwag and declares which orders lines it can call
// (depends), so the gateway knows orders v1 still has a user and will
// not call v1 sunset-able while billing accepts only v1.
//
// Moving billing to v2 is a code change on billing's side (the v2 path
// below) plus a new depends entry; ORDERS_LINE picks one here so the
// walkthrough can run both.
//
//   GATEWAY=http://localhost:8080 CONTROL_PLANE=localhost:50090 ORDERS_LINE=v1 pnpm dependent
import { createServer } from "node:http";
import type { AddressInfo } from "node:net";
import { register } from "@gwag/registration";
import createClient from "openapi-fetch";
import type { paths as V1 } from "./gen/orders-v1.ts";
import type { paths as V2 } from "./gen/orders-v2.ts";

const gateway = process.env.GATEWAY ?? "http://localhost:8080";
const controlPlane = process.env.CONTROL_PLANE ?? "localhost:50090";
const line = process.env.ORDERS_LINE ?? "v1";
if (line !== "v1" && line !== "v2") throw new Error(`ORDERS_LINE=${line}: want v1 or v2`);

const headers = { "X-Caller-Id": "billing" };
const ordersV1 = createClient<V1>({ baseUrl: `${gateway}/api/ingress`, headers });
const ordersV2 = createClient<V2>({ baseUrl: `${gateway}/api/ingress`, headers });

// Amount due for an order, in cents, on whichever line billing uses.
async function dueCents(id: string): Promise<number> {
  if (line === "v1") {
    const r = await ordersV1.GET("/v1/orders/{id}", { params: { path: { id } } });
    if (!r.data) throw new Error(`orders v1 getOrder ${id}: HTTP ${r.response.status}`);
    return Math.round(r.data.amount * 100); // v1 has dollars and no refunds
  }
  const r = await ordersV2.GET("/v2/orders/{id}", { params: { path: { id } } });
  if (!r.data) throw new Error(`orders v2 getOrder ${id}: HTTP ${r.response.status}`);
  return r.data.amountCents - r.data.refundedCents;
}

// billing's own API, served so the gateway has something to route to.
const spec = {
  openapi: "3.0.3",
  info: { title: "billing", version: "1.0.0" },
  paths: {
    "/billing/v1/due/{id}": {
      get: {
        operationId: "dueCents",
        parameters: [{ name: "id", in: "path", required: true, schema: { type: "string" } }],
        responses: {
          "200": {
            description: "Amount due",
            content: {
              "application/json": {
                schema: { type: "object", properties: { cents: { type: "integer" } }, required: ["cents"] },
              },
            },
          },
        },
      },
    },
  },
};

const server = createServer(async (req, res) => {
  const m = /^\/billing\/v1\/due\/([^/?]+)/.exec(req.url ?? "");
  if (req.method !== "GET" || !m || !m[1]) {
    res.writeHead(404).end();
    return;
  }
  try {
    const cents = await dueCents(decodeURIComponent(m[1]));
    res.writeHead(200, { "Content-Type": "application/json" }).end(JSON.stringify({ cents }));
  } catch (e) {
    res.writeHead(502, { "Content-Type": "application/json" }).end(JSON.stringify({ error: String(e) }));
  }
});
await new Promise<void>((resolve) => server.listen(Number(process.env.PORT ?? 0), "127.0.0.1", resolve));
const port = (server.address() as AddressInfo).port;

const reg = await register({
  gateway: controlPlane,
  serviceAddr: `http://127.0.0.1:${port}`,
  instanceId: `billing@${port}`,
  deregisterOnSignal: ["SIGTERM", "SIGINT"],
  services: [{
    namespace: "billing",
    version: "v1",
    openapiSpec: JSON.stringify(spec),
    depends: [{ service: "orders", lines: [line] }],
    owner: { team: "billing", contact: "#billing" },
    public: true,
  }],
});
console.log(`billing registered (${reg.id}), depends on orders ${line}`);
console.log(`billing due o-1 via orders ${line}: ${await dueCents("o-1")} cents`);
