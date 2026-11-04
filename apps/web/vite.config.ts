import react from "@vitejs/plugin-react";
import { defineConfig } from "vitest/config";

// The API port-forwarded from kind (make kind-up); kind's api.siteOrigin.
const api = process.env.HEARTBEAT_API_URL ?? "http://localhost:8081";

// ADR 0020: the API embeds the build from dist/app and serves it at /. In
// development Vite serves the app and proxies the API's paths to it, sending
// the API's own origin so its Origin check passes unchanged.
const proxied = { target: api, changeOrigin: true, headers: { origin: api } };

export default defineConfig({
  plugins: [react()],
  build: {
    outDir: "dist/app",
    emptyOutDir: true,
    // Hashed file names under assets/, which the API caches as immutable.
    assetsDir: "assets",
  },
  server: {
    port: 5173,
    strictPort: true,
    proxy: { "/api": proxied, "/healthz": proxied, "/readyz": proxied },
  },
  test: {
    environment: "jsdom",
    setupFiles: ["src/test/setup.ts"],
    css: false,
  },
});
