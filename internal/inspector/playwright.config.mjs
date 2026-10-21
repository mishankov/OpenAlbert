import { defineConfig } from "../../docs/node_modules/@playwright/test/index.mjs";
import { fileURLToPath } from "node:url";
export default defineConfig({
  testDir: "./tests",
  outputDir: "./test-results",
  fullyParallel: false,
  use: {
    baseURL: "http://127.0.0.1:4181",
    viewport: { width: 1440, height: 1000 },
    launchOptions: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH
      ? { executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH }
      : {},
  },
  webServer: {
    command: "node tests/server.mjs",
    cwd: fileURLToPath(new URL(".", import.meta.url)),
    url: "http://127.0.0.1:4181/inspect/",
    reuseExistingServer: !process.env.CI,
  },
});
