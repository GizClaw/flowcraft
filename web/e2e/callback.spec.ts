import type { Page } from '@playwright/test';
import { test, expect } from './fixtures';

async function ensureMockE2ESetup(page: Page) {
  await page.goto('/setup');
  const heading = page.getByText('Welcome to FlowCraft');
  if (await heading.isVisible({ timeout: 3000 }).catch(() => false)) {
    const providerSelect = page.locator('select').first();
    await providerSelect.selectOption('mock');
    await page.getByPlaceholder('sk-...').fill('mock-key');
    const modelSelect = page.locator('select').nth(1);
    await modelSelect.selectOption('mock-e2e');
    await page.getByRole('button', { name: 'Get Started' }).click();
    await expect(page).toHaveURL(/\/agents/, { timeout: 10_000 });
  }
}

async function ensureWorkerAgent(page: Page): Promise<string> {
  return page.evaluate(async () => {
    const headers = { 'Content-Type': 'application/json' };

    const listRes = await fetch('/api/agents', { headers, credentials: 'include' });
    const list = await listRes.json() as { data?: Array<{ id: string; name: string }> };
    const existing = (list.data || []).find((a) => a.name === 'E2E Callback Worker');
    if (existing) return existing.id;

    const createRes = await fetch('/api/agents', {
      method: 'POST',
      headers,
      credentials: 'include',
      body: JSON.stringify({
        name: 'E2E Callback Worker',
        type: 'workflow',
        config: {},
        graph_definition: {
          name: 'e2e-callback-worker',
          entry: 'run',
          nodes: [
            {
              id: 'run',
              type: 'script',
              config: {
                source: `
                  var q = String(board.getVar("query") || "");
                  board.setVar("response", "Worker completed: " + q);
                `,
              },
            },
            {
              id: 'out',
              type: 'answer',
              config: {
                keys: ['response'],
              },
            },
          ],
          edges: [
            { from: 'run', to: 'out' },
            { from: 'out', to: '__end__' },
          ],
        },
      }),
    });
    if (!createRes.ok) {
      throw new Error(`create worker failed: ${createRes.status} ${await createRes.text()}`);
    }
    const created = await createRes.json() as { id: string };
    return created.id;
  });
}

test.describe('Callback Flow', () => {
  test.beforeEach(async ({ page }) => {
    await ensureMockE2ESetup(page);
  });

  test('copilot dispatch reaches callback UI through real protocol chain', async ({ page }) => {
    const workerID = await ensureWorkerAgent(page);
    const pageErrors: Error[] = [];
    page.on('pageerror', (err) => pageErrors.push(err));

    await page.goto('/agents');
    await page.getByTitle('Open CoPilot').click();
    await expect(page.getByText('CoPilot')).toBeVisible({ timeout: 10_000 });

    const input = page.getByPlaceholder('Ask CoPilot... (@ to mention nodes)');
    await input.fill(`[E2E_DISPATCH target=${workerID}] build a callback verification example`);
    await input.press('Enter');

    await expect(page.getByText('E2E dispatch submitted. Waiting for callback.')).toBeVisible({ timeout: 15_000 });
    await expect(page.getByText(/\[Task Callback\]/).first()).toBeVisible({ timeout: 15_000 });
    await expect(page.getByText('E2E callback processed successfully.')).toBeVisible({ timeout: 15_000 });

    await page.getByRole('button', { name: 'Tasks' }).click();
    await expect(page.getByText('E2E Callback Worker')).toBeVisible({ timeout: 10_000 });
    await expect(page.getByText('Completed')).toBeVisible({ timeout: 10_000 });

    expect(pageErrors).toEqual([]);
  });
});
