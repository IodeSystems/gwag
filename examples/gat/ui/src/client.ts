import { GraphQLClient } from 'graphql-request'

// Single-page apps usually point at "/api/graphql" relative; Vite's
// dev proxy in vite.config.ts forwards to localhost:8080.
export const client = new GraphQLClient('/api/graphql')
