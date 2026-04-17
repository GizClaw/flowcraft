import { test, expect } from './fixtures';

test.describe('Setup', () => {
  test('shows account setup page on first visit', async ({ page }) => {
    await page.goto('/setup');
    await expect(page.getByText('Welcome to FlowCraft')).toBeVisible({ timeout: 10_000 });
  });

  test('creates admin account and configures provider', async ({ page }) => {
    await page.goto('/setup');
    await expect(page.getByText('Welcome to FlowCraft')).toBeVisible({ timeout: 10_000 });

    // Fill admin account form
    await page.getByPlaceholder('admin').fill('admin');
    await page.getByPlaceholder('At least 6 characters').fill('test-password');
    await page.getByPlaceholder('Re-enter password').fill('test-password');
    await page.getByRole('button', { name: 'Create Admin Account' }).click();

    // Should move to provider configuration step
    await expect(page.getByText('Configure LLM Provider')).toBeVisible({ timeout: 10_000 });

    // Select "mock" provider
    const providerSelect = page.locator('select').first();
    await providerSelect.selectOption('mock');

    // Fill provider API key
    await page.getByPlaceholder('sk-...').fill('mock-key');

    // Select model
    const modelSelect = page.locator('select').nth(1);
    await modelSelect.selectOption('mock-default');

    // Click Get Started
    await page.getByRole('button', { name: 'Get Started' }).click();

    // Should navigate to agents
    await expect(page).toHaveURL(/\/agents/, { timeout: 10_000 });
  });
});
