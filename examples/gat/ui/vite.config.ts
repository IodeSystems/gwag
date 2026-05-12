import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// Vite proxies /api to the Go server so the UI can be served on
// 5173 while the gat server runs on 8080. Adjust target if you
// boot the server on a different port.
export default defineConfig({
  plugins: [react()],
  server: {
    proxy: {
      '/api': 'http://localhost:8080',
      '/projects': 'http://localhost:8080',
    },
  },
})
