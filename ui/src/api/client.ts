// Thin wrapper around graphql-request that points at the gateway's
// /graphql endpoint. Used by every page through the generated SDK
// (src/api/gateway.ts, produced by `pnpm run codegen`).
//
// In dev, the Vite proxy forwards /graphql to GATEWAY_URL (default
// http://localhost:18080). In prod, the UI bundle is served by the
// gateway itself so /graphql is same-origin.

import { GraphQLClient } from 'graphql-request';
import { getSdk } from './gateway';
import { getAdminToken } from './auth';

export const client = new GraphQLClient('/graphql', {
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

export const sdk = getSdk(client);
