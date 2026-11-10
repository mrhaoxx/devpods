import { defineConfig, devices } from "@playwright/test";

// Runs on the cluster via hack/e2e-ui.sh (never locally): the mock API
// serves the built SPA on :8080.
export default defineConfig({
  testDir: ".",
  timeout: 30_000,
  reporter: [["list"]],
  use: {
    ...devices["Desktop Chrome"],
    baseURL: "http://localhost:8080",
    viewport: { width: 1440, height: 900 },
    locale: "zh-CN",
  },
  webServer: { command: "node mock-server.mjs", url: "http://localhost:8080/healthz", reuseExistingServer: false },
});
