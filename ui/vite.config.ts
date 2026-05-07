import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';
import { tanstackRouter } from '@tanstack/router-plugin/vite';
import path from 'node:path';

const GATEWAY_URL = process.env.GATEWAY_URL || 'http://localhost:18080';

export default defineConfig({
  plugins: [
    tanstackRouter({ routesDirectory: 'src/routes', generatedRouteTree: 'src/routeTree.gen.ts' }),
    react(),
  ],
  resolve: {
    alias: {
      '@': path.resolve(__dirname, './src'),
    },
  },
  server: {
    port: 5173,
    proxy: {
      // Single /api/* prefix — graphql, schema, admin, metrics,
      // health all live under it. ws: true upgrades subscriptions.
      '/api': { target: GATEWAY_URL, changeOrigin: true, ws: true },
    },
  },
});
