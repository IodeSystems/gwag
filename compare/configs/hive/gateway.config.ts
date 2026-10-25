// Hive Gateway runtime config for the perf comparison.
//
// Production-like: no GraphiQL, no landing page, no plugins. Port and
// supergraph path come from start-hive.sh CLI flags.
import { defineConfig } from '@graphql-hive/gateway';

export const gatewayConfig = defineConfig({
  graphiql: false,
  landingPage: false,
  // Keep per-request logging out of the hot path.
  logging: 'warn',
});
