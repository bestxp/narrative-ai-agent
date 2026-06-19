import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// Vite config for the wschat dev client.
// - build.outDir writes into ./dist (inside web/) so the Go embed
//   directive (`//go:embed web/dist`) picks up the production
//   bundle. The Go embed paths are relative to the .go file in
//   internal/messaging/wschat/, so web/dist resolves correctly.
// - dev server proxies /ws and /api/commands to the Go server so
//   hot-reload works without CORS tweaks: run `go run ./cmd/bot`
//   (serving the API on 127.0.0.1:8090) and `npm run dev` (Vite on
//   5173) side by side. Open http://localhost:5173 in dev, or
//   http://127.0.0.1:8090 for the embedded build.
export default defineConfig({
  plugins: [react()],
  build: {
    outDir: 'dist',
    emptyOutDir: true,
  },
  server: {
    port: 5173,
    proxy: {
      '/ws': { target: 'ws://127.0.0.1:8090', ws: true },
      '/api': 'http://127.0.0.1:8090',
    },
  },
})