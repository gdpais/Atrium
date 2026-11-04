import { defineConfig, devices } from "@playwright/test";
import { readFileSync } from "node:fs";
import type { Options } from "./e2e/fixtures";

// Playwright drives the built UI against the real API (ADR 0020 §6):
// scripts/kind-e2e.sh port-forwards the API to http://localhost:8081, kind's
// api.siteOrigin, so the browser's Origin is the one the API accepts, and
// creates the users whose emails and password files it passes here.
function account(role: "VIEWER" | "OPERATOR") {
  const passwordFile = process.env[`HEARTBEAT_E2E_${role}_PASSWORD_FILE`];
  return {
    email: process.env[`HEARTBEAT_E2E_${role}_EMAIL`] ?? "",
    password: passwordFile ? readFileSync(passwordFile, "utf8").trim() : "",
  };
}

export default defineConfig<Options>({
  testDir: "e2e",
  outputDir: process.env.HEARTBEAT_E2E_OUTPUT ?? "test-results",
  fullyParallel: false,
  workers: 1,
  retries: 0,
  forbidOnly: true,
  timeout: 30_000,
  reporter: [["list"]],
  use: {
    baseURL: process.env.HEARTBEAT_UI_URL ?? "http://localhost:8081",
    trace: "retain-on-failure",
    viewer: account("VIEWER"),
    operator: account("OPERATOR"),
  },
  projects: [{ name: "chromium", use: { ...devices["Desktop Chrome"] } }],
});
