import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react-swc';
import { tanstackRouter } from '@tanstack/router-plugin/vite';
import path from 'node:path';
import { writeFileSync } from 'node:fs';

const GATEWAY_URL = process.env.GATEWAY_URL || 'http://localhost:18080';

// @vitejs/plugin-react-swc replaces @vitejs/plugin-react (Babel) —
// SWC is significantly faster on cold builds and during test
// transformation.
//
// We considered graphql-tag-swc-plugin to compile `gql\`...\``
// template literals to pre-parsed AST objects at build time, but its
// WASM ABI lags @vitejs/plugin-react-swc's bundled @swc/core (the
// plugin's published 2.0.0 wasm targets swc_core 16, current
// react-swc is on 32-era). Runtime parse via graphql-tag fires once
// per query at module init — microseconds for our four-or-so
// queries — so the precompile isn't worth the toolchain juggling.
// Revisit if the plugin becomes ABI-current with a newer SWC.
// Vite empties dist/ before a build. dist/.gitkeep is tracked so that
// //go:embed all:dist compiles in a module-proxy build (ui/embed.go);
// put it back so a build leaves the git tree clean.
const keepDist = () => ({
  name: 'keep-dist-gitkeep',
  closeBundle() {
    writeFileSync(path.resolve(import.meta.dirname, 'dist/.gitkeep'), '');
  },
});

export default defineConfig({
  plugins: [
    keepDist(),
    tanstackRouter({ routesDirectory: 'src/routes', generatedRouteTree: 'src/routeTree.gen.ts' }),
    react(),
  ],
  resolve: {
    alias: {
      '@': path.resolve(import.meta.dirname, './src'),
    },
  },
  server: {
    port: 5173,
    proxy: {
      // Single /api/* prefix — graphql, schema, admin, metrics,
      // health all live under it. ws: true upgrades subscriptions.
      '/api': { target: GATEWAY_URL, changeOrigin: true, ws: true },
      // SSO sign-in (--sso-issuer). No changeOrigin: the gateway builds
      // the callback URL from Host, which must stay the dev server's.
      '/auth': { target: GATEWAY_URL },
    },
  },
});
