// A TypeScript client on orders v2. Same gateway, same service process
// as old-client.ts; types from the v2 snapshot.
//
//   GATEWAY=http://localhost:8080 pnpm new-client
import createClient from "openapi-fetch";
import type { paths } from "./gen/orders-v2.ts";

const gateway = process.env.GATEWAY ?? "http://localhost:8080";
const orders = createClient<paths>({
  baseUrl: `${gateway}/api/ingress`,
  headers: { "X-Caller-Id": process.env.CALLER ?? "new-ts-client" },
});

const list = await orders.GET("/v2/orders", { params: { query: { currency: "EUR" } } });
if (!list.data) throw new Error(`v2 listOrders: HTTP ${list.response.status}`);
for (const o of list.data.orders ?? []) {
  console.log(`v2 listOrders EUR: ${o.id} amountCents=${o.amountCents} currency=${o.currency} customer=${o.customer}`);
}
