// Thin wrapper around graphql-request that points at the gateway's
// GraphQL endpoint. Each page imports the typed operation documents
// from `./operations` and dispatches via `client.request(doc, vars)`
// — graphql-request 7.x consumes TypedDocumentNode directly, so the
// variables and result types flow through automatically.
//
// All gateway routes live under `/api/*` so the UI bundle owns the
// SPA root. In dev, the Vite proxy forwards `/api` to GATEWAY_URL
// (default http://localhost:18080). In prod, the UI bundle is
// served by the gateway itself so `/api/graphql` is same-origin.

import { GraphQLClient } from 'graphql-request';
import { getAdminToken } from './auth';

// graphql-request 7.x calls `new URL(endpoint)` per request — that
// throws on a bare relative path ("/api/graphql") in browsers since
// URL needs a base for relative paths. Build the absolute URL from
// window.location at startup; fall back to a placeholder for SSR /
// non-window contexts (codegen tests, etc.).
const endpoint =
  typeof window === 'undefined'
    ? 'http://localhost/api/graphql'
    : `${window.location.origin}/api/graphql`;

export const client = new GraphQLClient(endpoint, {
  // Lazy header function — re-evaluated per request, so updates to the
  // sessionStorage-backed token take effect immediately without
  // recreating the client. admin_* mutations dispatch through the
  // gateway's /admin/* path, which gates writes on the bearer.
  headers: () => {
    const token = getAdminToken();
    const h: Record<string, string> = {};
    if (token) h.Authorization = `Bearer ${token}`;
    return h;
  },
});
