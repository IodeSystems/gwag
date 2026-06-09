import { GraphQLClient } from 'graphql-request'

// graphql-request v7 constructs `new URL(endpoint)`, which rejects a bare
// relative path, so anchor to the page origin. Still same-origin, so Vite's
// dev proxy (vite.config.ts) forwards /api to localhost:8080.
export const client = new GraphQLClient(`${window.location.origin}/api/graphql`)
