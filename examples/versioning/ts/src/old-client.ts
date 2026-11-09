// A TypeScript client still on orders v1. Types come from the frozen v1
// snapshot (gen.sh), calls go through the gateway's REST ingress.
//
//   GATEWAY=http://localhost:8080 pnpm old-client
import createClient from "openapi-fetch";
import type { paths } from "./gen/orders-v1.ts";

const gateway = process.env.GATEWAY ?? "http://localhost:8080";
const orders = createClient<paths>({
  baseUrl: `${gateway}/api/ingress`,
  headers: { "X-Caller-Id": process.env.CALLER ?? "old-ts-client" },
});

const one = await orders.GET("/v1/orders/{id}", { params: { path: { id: "o-2" } } });
if (!one.data) throw new Error(`v1 getOrder: HTTP ${one.response.status}`);
// one.data.amount is dollars; there is no currency field in v1.
console.log(`v1 getOrder o-2: amount=${one.data.amount.toFixed(2)} status=${one.data.status}`);

const eur = await orders.GET("/v1/orders/{id}", { params: { path: { id: "o-3" } } });
// The gateway's REST ingress keeps the upstream status but wraps the
// body as {"error": "..."}; the service's own body is {"detail": "..."}.
const err = eur.error as { detail?: string; error?: string } | undefined;
console.log(`v1 getOrder o-3: HTTP ${eur.response.status} ${err?.detail ?? err?.error ?? ""}`);
