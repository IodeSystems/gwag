// Minimal GraphQL client that points at the gateway's GraphQL
// endpoint. Each page imports the typed operation documents from
// `./operations` and dispatches via `client.request(doc, vars)` —
// documents are TypedDocumentNode, so the variables and result types
// flow through automatically.
//
// All gateway routes live under `/api/*` so the UI bundle owns the
// SPA root. In dev, the Vite proxy forwards `/api` to GATEWAY_URL
// (default http://localhost:18080). In prod, the UI bundle is
// served by the gateway itself so `/api/graphql` is same-origin.
//
// This replaced graphql-request, which does not support graphql 17.

import type { TypedDocumentNode } from '@graphql-typed-document-node/core';
import { print } from 'graphql';
import { getAdminToken } from './auth';

const endpoint = '/api/graphql';

type GraphQLResponse<T> = {
  data?: T | null;
  errors?: { message: string }[];
};

// Operations with no variables take no second argument.
type VariablesArg<V> = V extends Record<string, never> ? [variables?: V] : [variables: V];

async function request<TResult, TVariables>(
  doc: TypedDocumentNode<TResult, TVariables>,
  ...[variables]: VariablesArg<TVariables>
): Promise<TResult> {
  const headers: Record<string, string> = { 'Content-Type': 'application/json' };
  // Read per request, so a token change takes effect immediately.
  // admin_* mutations dispatch through the gateway's /admin/* path,
  // which gates writes on the bearer.
  const token = getAdminToken();
  if (token) headers.Authorization = `Bearer ${token}`;

  const res = await fetch(endpoint, {
    method: 'POST',
    headers,
    body: JSON.stringify({ query: print(doc), variables }),
  });
  let body: GraphQLResponse<TResult>;
  try {
    body = (await res.json()) as GraphQLResponse<TResult>;
  } catch {
    throw new Error(`GraphQL request failed: HTTP ${res.status}`);
  }
  if (body.errors?.length) {
    throw new Error(body.errors.map((e) => e.message).join('; '));
  }
  if (!res.ok || body.data == null) {
    throw new Error(`GraphQL request failed: HTTP ${res.status}`);
  }
  return body.data;
}

export const client = { request };
