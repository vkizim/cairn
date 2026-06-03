import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'

// Dev server on :5173 proxies /api to the Go backend on :8080 so the browser
// sees a single origin and the session/CSRF cookies work same-origin. The
// backend must run with CAIRN_DEV=1 so the cookie is not marked Secure (plain
// http on localhost). Build output (dist/) is embedded into the Go binary with
// `-tags embed_spa`.
export default defineConfig({
  plugins: [react(), tailwindcss()],
  server: {
    port: 5173,
    proxy: {
      '/api': {
        target: 'http://localhost:8080',
        changeOrigin: false,
      },
    },
  },
})
