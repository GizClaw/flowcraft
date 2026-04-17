import { defineConfig } from '@playwright/test';

const CI = !!process.env.CI;

export default defineConfig({
  testDir: './e2e',
  timeout: 60_000,
  expect: { timeout: 10_000 },
  retries: CI ? 2 : 0,
  workers: 1,
  reporter: CI ? 'github' : 'list',
  use: {
    baseURL: 'http://localhost:5173',
    headless: true,
    screenshot: 'only-on-failure',
    trace: 'on-first-retry',
  },
  webServer: [
    {
      command:
        'cd .. && rm -rf .playwright-home && export HOME="$PWD/.playwright-home" && mkdir -p "$HOME/.flowcraft" && go run ./cmd/flowcraft server',
      port: 8080,
      reuseExistingServer: !CI,
      timeout: 30_000,
      env: {
        FLOWCRAFT_AUTH_API_KEY: 'e2e-test-key',
      },
    },
    {
      command: 'npm run dev',
      port: 5173,
      reuseExistingServer: !CI,
      timeout: 15_000,
    },
  ],
});
