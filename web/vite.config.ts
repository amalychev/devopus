import { readFileSync } from "node:fs"
import { homedir } from "node:os"
import { join, resolve } from "node:path"
import tailwindcss from "@tailwindcss/vite"
import react from "@vitejs/plugin-react"
import { defineConfig } from "vite"

// npm run dev talks to the running local panel (devopus panel start): its port and
// token are in $DEVOPUS_HOME/panel (default ~/.devopus/panel).
function agentPanel() {
  const home = join(process.env.DEVOPUS_HOME || join(homedir(), ".devopus"), "panel")
  try {
    return {
      port: readFileSync(join(home, "port"), "utf8").trim(),
      token: readFileSync(join(home, "token"), "utf8").trim(),
    }
  } catch {
    return undefined
  }
}

export default defineConfig(({ command }) => {
  const panel = command === "serve" ? agentPanel() : undefined
  return {
    plugins: [react(), tailwindcss()],
    resolve: {
      alias: {
        "@": resolve(import.meta.dirname, "./src"),
      },
    },
    build: {
      // Built into the devopus binary (internal/panel, go:embed).
      outDir: resolve(import.meta.dirname, "../internal/panel/dist"),
      emptyOutDir: true,
      chunkSizeWarningLimit: 1024,
    },
    server: panel && {
      proxy: {
        "/devopus.agent.v1.AgentService": {
          target: `http://127.0.0.1:${panel.port}`,
          changeOrigin: true,
          headers: { Authorization: `Bearer ${panel.token}` },
          configure: (proxy) => {
            proxy.on("proxyReq", (req) => req.removeHeader("origin"))
          },
        },
      },
    },
  }
})
