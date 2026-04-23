import { test, expect } from './fixtures';

test.describe('Kanban', () => {
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

  test('loads kanban page and connects runtime websocket', async ({ page }) => {
    const pageErrors: Error[] = [];
    page.on('pageerror', (err) => pageErrors.push(err));

    const wsTicketReq = page.waitForRequest((req) =>
      req.url().includes('/api/ws-ticket') && req.method() === 'POST',
    );
    const runtimeStatsResp = page.waitForResponse((resp) =>
      resp.url().includes('/api/stats/runtime') && resp.request().method() === 'GET',
    );
    const memoryStatsResp = page.waitForResponse((resp) =>
      resp.url().includes('/api/stats/memory') && resp.request().method() === 'GET',
    );
    const kanbanCardsResp = page.waitForResponse((resp) =>
      resp.url().includes('/api/kanban/cards') && resp.request().method() === 'GET',
    );
    const websocket = page.waitForEvent('websocket', (ws) => ws.url().includes('/api/events/ws'));

    await page.goto('/kanban');

    const [ticketReq, runtimeResp, memoryResp, cardsResp, ws] = await Promise.all([
      wsTicketReq,
      runtimeStatsResp,
      memoryStatsResp,
      kanbanCardsResp,
      websocket,
    ]);

    expect(ticketReq.headers()['x-user-id']).toBeTruthy();
    expect(runtimeResp.ok()).toBeTruthy();
    expect(memoryResp.ok()).toBeTruthy();
    expect(cardsResp.ok()).toBeTruthy();
    expect(ws.url()).toContain('/api/events/ws?ticket=');

    await expect(page.getByText('Runtime Debug')).toBeVisible({ timeout: 10_000 });
    await expect(page.getByText(/user:\s*default/)).toBeVisible();
    await expect(page.getByRole('button', { name: 'Board' })).toBeVisible();
    await expect(page.getByRole('button', { name: 'Timeline' })).toBeVisible();
    await expect(page.getByRole('button', { name: 'Topology' })).toBeVisible();

    await page.getByRole('button', { name: 'Timeline' }).click();
    await expect(page.getByRole('button', { name: 'Timeline' })).toHaveClass(/bg-indigo-100|bg-indigo-900/);

    await page.getByRole('button', { name: 'Topology' }).click();
    await expect(page.getByRole('button', { name: 'Topology' })).toHaveClass(/bg-indigo-100|bg-indigo-900/);

    expect(pageErrors).toEqual([]);
  });
});
