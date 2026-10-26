import { defineConfig } from "../../docs/node_modules/@playwright/test/index.mjs";
import { fileURLToPath } from "node:url";
const port = Number(process.env.INSPECTOR_TEST_PORT || 4181);
const baseURL = `http://127.0.0.1:${port}`;
export default defineConfig({
  testDir: "./tests",
  outputDir: "./test-results",
  fullyParallel: false,
  use: {
    baseURL,
    viewport: { width: 1440, height: 1000 },
    trace: "retain-on-failure",
    screenshot: "only-on-failure",
    launchOptions: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH
      ? { executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH }
      : {},
  },
  webServer: {
    command: "node tests/server.mjs",
    cwd: fileURLToPath(new URL(".", import.meta.url)),
    url: `${baseURL}/inspect/`,
    reuseExistingServer: false,
  },
});
