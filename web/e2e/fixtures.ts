import { test as base, expect } from '@playwright/test';

const API_KEY = process.env.FLOWCRAFT_AUTH_API_KEY || 'e2e-test-key';

export const test = base.extend({
  page: async ({ page }, use) => {
    await page.request.post('/api/auth/login', {
      data: { api_key: API_KEY },
    });
    await use(page);
  },
});

export { expect };
