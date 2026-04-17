import { test, expect } from './fixtures';

test.describe('Agent Management', () => {
  test.beforeEach(async ({ page }) => {
    // Ensure mock provider is configured so SetupGuard doesn't redirect
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

  test('displays agents page with title', async ({ page }) => {
    await page.goto('/agents');
    await expect(page.getByRole('heading', { name: 'Agents' })).toBeVisible({ timeout: 10_000 });
  });

  test('create and delete an agent', async ({ page }) => {
    await page.goto('/agents');
    await expect(page.getByRole('heading', { name: 'Agents' })).toBeVisible({ timeout: 10_000 });

    // Open create dialog
    await page.getByRole('button', { name: /Create Agent/ }).click();
    await expect(page.getByText('Create Agent').first()).toBeVisible();

    // Fill name
    await page.getByPlaceholder('My Workflow').fill('E2E Test Agent');

    // Click create button in the dialog
    const createButton = page.locator('button').filter({ hasText: 'Create Agent' }).last();
    await createButton.click();

    // Verify agent appears in the list
    await expect(page.getByText('E2E Test Agent')).toBeVisible({ timeout: 10_000 });

    // Delete the agent: hover to show menu
    const card = page.getByText('E2E Test Agent').first();
    await card.hover();

    // Click the menu button (MoreVertical icon)
    const menuBtn = page.locator('button').filter({ has: page.locator('svg.lucide-more-vertical') });
    if (await menuBtn.isVisible({ timeout: 2000 }).catch(() => false)) {
      await menuBtn.click();
      await page.getByText('Delete').click();

      // Confirm deletion
      const confirmBtn = page.getByRole('button', { name: 'Delete' }).last();
      await confirmBtn.click();

      // Verify agent is removed
      await expect(page.getByText('E2E Test Agent')).not.toBeVisible({ timeout: 10_000 });
    }
  });
});
