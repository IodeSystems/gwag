// Thin wrapper around graphql-request that points at the gateway's
// /graphql endpoint. Used by every page through the generated SDK
// (src/api/gateway.ts, produced by `pnpm run codegen`).
//
// In dev, the Vite proxy forwards /graphql to GATEWAY_URL (default
// http://localhost:18080). In prod, the UI bundle is served by the
// gateway itself so /graphql is same-origin.

import { GraphQLClient } from 'graphql-request';
import { getSdk } from './gateway';

export const client = new GraphQLClient('/graphql', {
  // Headers added here are forwarded on every request — pair with
  // a server-side auth pass-through middleware (see plan.md tier-1).
});

export const sdk = getSdk(client);
