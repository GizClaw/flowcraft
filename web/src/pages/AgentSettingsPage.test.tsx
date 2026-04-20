import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, fireEvent, waitFor } from '@testing-library/react';
import { MemoryRouter, Routes, Route } from 'react-router-dom';
import AgentSettingsPage from './AgentSettingsPage';
import { agentApi, channelApi, skillApi } from '../utils/api';
import type { Agent, UpdateAgentRequest } from '../types/app';

const { addToast } = vi.hoisted(() => ({ addToast: vi.fn() }));

vi.mock('../store/toastStore', () => {
  const state = { addToast };
  const useToastStore = (selector?: (s: typeof state) => unknown) =>
    selector ? selector(state) : state;
  (useToastStore as unknown as { getState: () => typeof state }).getState = () => state;
  return { useToastStore };
});

const { stableI18n } = vi.hoisted(() => {
  const t = (key: string, params?: Record<string, unknown>) => {
    if (params && Object.keys(params).length > 0) {
      const parts = Object.entries(params).map(([k, v]) => `${k}=${String(v)}`);
      return `${key}(${parts.join(',')})`;
    }
    return key;
  };
  return { stableI18n: { t } };
});
vi.mock('react-i18next', () => ({ useTranslation: () => stableI18n }));

// Stub heavy editor children so we focus the test on the save payload shape.
vi.mock('../components/variable/SchemaEditor', () => ({
  default: () => <div data-testid="schema-editor" />,
}));
vi.mock('../components/agents/SkillWhitelistEditor', () => ({
  default: () => <div data-testid="skill-whitelist-editor" />,
}));
vi.mock('../components/agents/LtmCategoryCheckboxGroup', () => ({
  default: () => <div data-testid="ltm-checkbox-group" />,
}));
vi.mock('./AgentMemoryPage', () => ({
  default: () => <div data-testid="memory-page" />,
}));

type Mocked = ReturnType<typeof vi.fn>;
const asMock = (fn: unknown) => fn as unknown as Mocked;

function makeAgent(overrides: Partial<Agent['config']> = {}): Agent {
  return {
    id: 'a1',
    name: 'demo',
    type: 'workflow',
    description: '',
    created_at: '2024-01-01',
    updated_at: '2024-01-01',
    config: {
      ...overrides,
    },
  };
}

beforeEach(() => {
  addToast.mockReset();
  vi.spyOn(channelApi, 'types').mockResolvedValue([]);
  vi.spyOn(skillApi, 'list').mockResolvedValue([]);
  vi.spyOn(agentApi, 'update').mockImplementation(async (_id, _body) => makeAgent());
});

afterEach(() => {
  vi.restoreAllMocks();
});

function renderWithAgent(agent: Agent) {
  const setAgent = vi.fn();
  render(
    <MemoryRouter initialEntries={['/']}>
      <Routes>
        <Route element={<OutletStub agent={agent} setAgent={setAgent} />}>
          <Route index element={<AgentSettingsPage />} />
        </Route>
      </Routes>
    </MemoryRouter>,
  );
  return { setAgent };
}

// Tiny outlet wrapper that injects the context AgentSettingsPage expects via useOutletContext.
import { Outlet } from 'react-router-dom';
function OutletStub({ agent, setAgent }: { agent: Agent; setAgent: (a: Agent) => void }) {
  return <Outlet context={{ agent, setAgent }} />;
}

async function clickSave() {
  fireEvent.click(screen.getByText('agentSettings.saveSettings'));
  await waitFor(() => expect(agentApi.update).toHaveBeenCalled());
}

function lastUpdatePayload(): UpdateAgentRequest {
  const calls = asMock(agentApi.update).mock.calls;
  expect(calls.length).toBeGreaterThan(0);
  return calls[calls.length - 1][1] as UpdateAgentRequest;
}

describe('AgentSettingsPage notification payload', () => {
  it('persists only {enabled: false} when notifications are disabled', async () => {
    // Even if the agent previously had a channel/granularity selected, saving
    // with the checkbox unticked should drop them so they do not silently
    // linger in the backing config.
    renderWithAgent(makeAgent({
      notification: { enabled: false, channel_name: 'slack', granularity: 'all' },
    }));

    await clickSave();
    const payload = lastUpdatePayload();
    expect(payload.config?.notification).toEqual({ enabled: false });
  });

  it('persists full notification payload when enabled', async () => {
    renderWithAgent(makeAgent({
      notification: { enabled: true, channel_name: 'slack', granularity: 'all' },
      channels: [{ type: 'slack', config: {} }],
    }));

    await clickSave();
    const payload = lastUpdatePayload();
    expect(payload.config?.notification).toEqual({
      enabled: true,
      channel_name: 'slack',
      granularity: 'all',
    });
  });

  it('toggling enabled off and saving strips channel_name', async () => {
    renderWithAgent(makeAgent({
      notification: { enabled: true, channel_name: 'slack', granularity: 'final' },
      channels: [{ type: 'slack', config: {} }],
    }));

    // Sanity: enable checkbox is currently checked. Click to disable.
    const checkbox = screen.getByLabelText('agentSettings.enableNotifications') as HTMLInputElement;
    expect(checkbox.checked).toBe(true);
    fireEvent.click(checkbox);
    expect(checkbox.checked).toBe(false);

    await clickSave();
    const payload = lastUpdatePayload();
    expect(payload.config?.notification).toEqual({ enabled: false });
  });

  it('preserves other config fields via spread (does not lose parallel)', async () => {
    renderWithAgent(makeAgent({
      parallel: { enabled: true, max_branches: 7, max_nesting: 2, merge_strategy: 'last_wins' },
      notification: { enabled: false },
    }));

    await clickSave();
    const payload = lastUpdatePayload();
    expect(payload.config?.parallel?.max_branches).toBe(7);
  });
});
