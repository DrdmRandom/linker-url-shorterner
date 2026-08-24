import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// Vite dev server config.
// `proxy` forwards /api and /s requests to the Go backend while you
// develop, so the frontend (port 5173) can talk to the backend (8080)
// without CORS headaches. In production both are served by the Go
// binary, so no proxy is involved.
export default defineConfig({
  plugins: [react()],
  server: {
    proxy: {
      '/api': 'http://localhost:8080',
      '/s': 'http://localhost:8080',
      '/auth': 'http://localhost:8080',
    },
  },
})
