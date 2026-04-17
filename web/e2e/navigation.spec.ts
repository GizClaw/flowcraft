import { test, expect } from './fixtures';

test.describe('Navigation', () => {
  test.beforeEach(async ({ page }) => {
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

  test('sidebar navigation works', async ({ page }) => {
    await page.goto('/agents');
    await expect(page.getByRole('heading', { name: 'Agents' })).toBeVisible({ timeout: 10_000 });

    // Navigate to Knowledge
    await page.getByRole('link', { name: 'Knowledge' }).click();
    await expect(page).toHaveURL(/\/knowledge/);
    await expect(page.getByRole('heading', { name: 'Knowledge Base' })).toBeVisible();

    // Navigate to Skills
    await page.getByRole('link', { name: 'Skills' }).click();
    await expect(page).toHaveURL(/\/skills/);

    // Navigate to Plugins
    await page.getByRole('link', { name: 'Plugins' }).click();
    await expect(page).toHaveURL(/\/plugins/);

    // Navigate back to Agents
    await page.getByRole('link', { name: 'Agents' }).click();
    await expect(page).toHaveURL(/\/agents/);
  });

  test('settings page accessible', async ({ page }) => {
    await page.goto('/global-settings');
    await expect(page.getByText('Settings')).toBeVisible({ timeout: 10_000 });
  });
});
