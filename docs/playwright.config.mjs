import { defineConfig } from '@playwright/test';
import { fileURLToPath } from 'node:url';

export default defineConfig({
  testDir: './site/tests/browser',
  outputDir: fileURLToPath(new URL('./test-results', import.meta.url)),
  fullyParallel: false,
  use: {
    baseURL: 'http://127.0.0.1:4173',
    viewport: { width: 1440, height: 1000 },
    launchOptions: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH
      ? { executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH } : {},
  },
  webServer: {
    command: 'npm run dev',
    cwd: fileURLToPath(new URL('.', import.meta.url)),
    url: 'http://127.0.0.1:4173',
    reuseExistingServer: !process.env.CI,
  },
});
