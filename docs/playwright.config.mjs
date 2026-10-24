import { defineConfig } from '@playwright/test';
import { fileURLToPath } from 'node:url';

const port = Number(process.env.DOCS_TEST_PORT || 4173);
const baseURL = `http://127.0.0.1:${port}`;

export default defineConfig({
  testDir: './site/tests/browser',
  outputDir: fileURLToPath(new URL('./test-results', import.meta.url)),
  fullyParallel: false,
  use: {
    baseURL,
    viewport: { width: 1440, height: 1000 },
    trace: 'retain-on-failure',
    screenshot: 'only-on-failure',
    launchOptions: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH
      ? { executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH } : {},
  },
  webServer: {
    command: 'npm run dev',
    cwd: fileURLToPath(new URL('.', import.meta.url)),
    url: baseURL,
    env: { PORT: String(port) },
    reuseExistingServer: !process.env.CI,
  },
});
