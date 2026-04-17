import { test, expect } from '@playwright/test';

test.describe('Login', () => {
  test('redirects to login when not authenticated', async ({ page }) => {
    await page.goto('/agents');
    await expect(page).toHaveURL(/\/(login|setup)/);
  });

  test('shows error on invalid credentials', async ({ page }) => {
    await page.goto('/login');
    await page.getByPlaceholder('Enter your username').fill('admin');
    await page.getByPlaceholder('Enter your password').fill('wrong-password');
    await page.getByRole('button', { name: 'Sign In' }).click();
    await expect(page.getByText(/Invalid|invalid|error/i)).toBeVisible({ timeout: 10_000 });
  });

  test('logs in with valid credentials', async ({ page }) => {
    await page.goto('/login');
    await page.getByPlaceholder('Enter your username').fill('admin');
    await page.getByPlaceholder('Enter your password').fill('e2e-test-pass');
    await page.getByRole('button', { name: 'Sign In' }).click();
    await expect(page).not.toHaveURL(/\/login/, { timeout: 10_000 });
  });

  test('keeps session after page reload', async ({ page }) => {
    await page.goto('/login');
    await page.getByPlaceholder('Enter your username').fill('admin');
    await page.getByPlaceholder('Enter your password').fill('e2e-test-pass');
    await page.getByRole('button', { name: 'Sign In' }).click();
    await expect(page).not.toHaveURL(/\/login/, { timeout: 10_000 });

    await page.reload();
    await expect(page).not.toHaveURL(/\/login/, { timeout: 10_000 });
  });
});
