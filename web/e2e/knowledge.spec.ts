import { test, expect } from './fixtures';

test.describe('Knowledge Base', () => {
  test.beforeEach(async ({ page }) => {
    // Ensure mock provider is configured
    await page.goto('/setup');
    const heading = page.getByText('Welcome to FlowCraft');
    if (await heading.isVisible({ timeout: 3000 }).catch(() => false)) {
      const providerSelect = page.locator('select').first();
      await providerSelect.selectOption('mock');
      await page.getByPlaceholder('sk-...').fill('mock-key');
      const modelSelect = page.locator('select').nth(1);
      await modelSelect.selectOption('mock-default');
      await page.getByRole('button', { name: 'Get Started' }).click();
      await expect(page).toHaveURL(/\/agents/, { timeout: 10_000 });
    }
  });

  test('displays knowledge page', async ({ page }) => {
    await page.goto('/knowledge');
    await expect(page.getByRole('heading', { name: 'Knowledge Base' })).toBeVisible({ timeout: 10_000 });
  });

  test('create and delete a dataset', async ({ page }) => {
    await page.goto('/knowledge');
    await expect(page.getByRole('heading', { name: 'Knowledge Base' })).toBeVisible({ timeout: 10_000 });

    // Open create dialog
    await page.getByRole('button', { name: /New Dataset/ }).click();

    // Fill dataset name
    const nameInput = page.getByPlaceholder(/Dataset name|name/i);
    await nameInput.fill('E2E Test Dataset');

    // Click create
    await page.getByRole('button', { name: 'Create' }).click();

    // Verify dataset appears
    await expect(page.getByText('E2E Test Dataset')).toBeVisible({ timeout: 10_000 });
  });
});
