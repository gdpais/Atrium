import react from "@vitejs/plugin-react";
import type { ProxyOptions } from "vite";
import { defineConfig } from "vitest/config";

// The API port-forwarded from kind (make kind-up); kind's api.siteOrigin.
const api = process.env.HEARTBEAT_API_URL ?? "http://localhost:8081";
// The origin the API accepts unsafe requests from (api.siteOrigin): the API's
// own address unless it is forwarded to another port.
const siteOrigin = process.env.HEARTBEAT_SITE_ORIGIN ?? api;
const port = 5173;
// The origins the dev server's own pages have.
const devOrigins = new Set([`http://localhost:${port}`, `http://127.0.0.1:${port}`]);

// ADR 0020: the API embeds the build from dist/app and serves it at /. In
// development Vite serves the app and proxies the API's paths to it. A
// request from the dev server's own page gets the site's origin, so the API's
// Origin check passes; any other keeps its own, so a page on another site
// still cannot make an unsafe request through the proxy.
const proxied: ProxyOptions = {
  target: api,
  changeOrigin: true,
  configure: (proxy) => {
    proxy.on("proxyReq", (proxyReq, req) => {
      if (devOrigins.has(req.headers.origin ?? "")) proxyReq.setHeader("origin", siteOrigin);
    });
  },
};

export default defineConfig({
  plugins: [react()],
  build: {
    outDir: "dist/app",
    emptyOutDir: true,
    // Hashed file names under assets/, which the API caches as immutable.
    assetsDir: "assets",
  },
  server: {
    port,
    strictPort: true,
    proxy: { "/api": proxied, "/healthz": proxied, "/readyz": proxied },
  },
  test: {
    environment: "jsdom",
    // e2e/ is Playwright's, run against the API (npm run e2e).
    include: ["src/**/*.test.{ts,tsx}"],
    setupFiles: ["src/test/setup.ts"],
    css: false,
  },
});
