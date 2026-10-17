import { resolve } from "node:path"
import tailwindcss from "@tailwindcss/vite"
import react from "@vitejs/plugin-react"
import { defineConfig } from "vite"

// The web interface of the workspace (R11), served by the workspace server. It shares
// the panel's components (src/components/ui) and styles.
//
// npm run dev:workspace talks to a running workspace server: devopus server run, at
// $DEVOPUS_SERVER_URL (default http://127.0.0.1:7481).
const server = process.env.DEVOPUS_SERVER_URL || "http://127.0.0.1:7481"

export default defineConfig({
  root: resolve(import.meta.dirname, "workspace"),
  plugins: [react(), tailwindcss()],
  resolve: {
    alias: {
      "@ws": resolve(import.meta.dirname, "./workspace/src"),
      "@": resolve(import.meta.dirname, "./src"),
    },
  },
  build: {
    // Built into the devopus binary (internal/server, go:embed).
    outDir: resolve(import.meta.dirname, "../internal/server/webdist"),
    emptyOutDir: true,
    chunkSizeWarningLimit: 1024,
  },
  server: {
    proxy: {
      "/devopus.workspace.v1.WorkspaceService": { target: server, changeOrigin: true },
      "/web/auth": { target: server, changeOrigin: true },
    },
  },
})
