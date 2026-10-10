import path from "node:path"
import { fileURLToPath } from "node:url"
import tailwindcss from "@tailwindcss/vite"
import { tanstackRouter } from "@tanstack/router-plugin/vite"
import react from "@vitejs/plugin-react"
import { defineConfig } from "vite"

const root = path.dirname(fileURLToPath(import.meta.url))

export default defineConfig({
  plugins: [tailwindcss(), tanstackRouter({ autoCodeSplitting: true }), react()],
  resolve: {
    alias: { "@": path.resolve(root, "src") },
  },
  server: {
    // The backend's control plane answers only on CONTROL_HOST, which in a
    // local checkout is localhost:2345. The dev server is on another port, so
    // the Host header has to be rewritten to get past onlyControlHost.
    //
    // The path is passed through unchanged: the backend serves its control
    // plane under /api itself, so stripping the prefix here forwards to a path
    // it does not serve and every request 404s.
    proxy: {
      "/api": {
        target: process.env.IRONHIDE_API_URL ?? "http://localhost:2345",
        changeOrigin: true,
      },
    },
  },
})
