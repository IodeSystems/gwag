import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';
import { TanStackRouterVite } from '@tanstack/router-plugin/vite';
import path from 'node:path';

const GATEWAY_URL = process.env.GATEWAY_URL || 'http://localhost:18080';

export default defineConfig({
  plugins: [
    TanStackRouterVite({ routesDirectory: 'src/routes', generatedRouteTree: 'src/routeTree.gen.ts' }),
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
      '/graphql': { target: GATEWAY_URL, changeOrigin: true, ws: true },
      '/schema': { target: GATEWAY_URL, changeOrigin: true },
      '/health': { target: GATEWAY_URL, changeOrigin: true },
    },
  },
});
