import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, fireEvent, waitFor } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import SettingsPage from './SettingsPage';
import { agentApi, channelApi, modelApi, skillApi } from '../utils/api';
import type { Agent, UpdateAgentRequest } from '../types/app';

// Focused regression coverage for the CoPilot section's handleSaveCoPilot —
// specifically that disabling notifications no longer carries channel_name
// and granularity forward in the persisted payload.

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

vi.mock('../components/agents/SkillWhitelistEditor', () => ({
  default: () => <div data-testid="skill-whitelist-editor" />,
}));
vi.mock('../components/agents/LtmCategoryCheckboxGroup', () => ({
  default: () => <div data-testid="ltm-checkbox-group" />,
}));

// authStore + uiStore — provide minimal stable state so the page mounts.
vi.mock('../store/authStore', () => {
  const state = { setAuthenticated: vi.fn(), setAccountSetup: vi.fn() };
  const useAuthStore = (selector?: (s: typeof state) => unknown) =>
    selector ? selector(state) : state;
  return { useAuthStore };
});
vi.mock('../store/uiStore', () => {
  const state = {
    theme: 'light',
    setTheme: vi.fn(),
    language: 'en',
    setLanguage: vi.fn(),
  };
  const useUIStore = (selector?: (s: typeof state) => unknown) =>
    selector ? selector(state) : state;
  return { useUIStore };
});

type Mocked = ReturnType<typeof vi.fn>;
const asMock = (fn: unknown) => fn as unknown as Mocked;

function makeCopilotAgent(notification: Agent['config']['notification']): Agent {
  return {
    id: 'copilot',
    name: 'CoPilot',
    type: 'copilot',
    description: '',
    created_at: '2024-01-01',
    updated_at: '2024-01-01',
    config: {
      notification,
      memory: { max_messages: 50 },
      channels: [],
      skill_whitelist: [],
    },
  };
}

beforeEach(() => {
  addToast.mockReset();
  vi.spyOn(modelApi, 'getProviders').mockResolvedValue([]);
  vi.spyOn(modelApi, 'list').mockResolvedValue([]);
  vi.spyOn(channelApi, 'types').mockResolvedValue([]);
  vi.spyOn(skillApi, 'list').mockResolvedValue([]);
  vi.spyOn(agentApi, 'update').mockImplementation(async (_id, _body) => makeCopilotAgent({ enabled: false }));
});

afterEach(() => {
  vi.restoreAllMocks();
});

async function expandCopilotSection(agent: Agent) {
  asMock(agentApi.get ?? (() => {})); // safety — replaced below
  vi.spyOn(agentApi, 'get').mockResolvedValue(agent);
  render(
    <MemoryRouter>
      <SettingsPage />
    </MemoryRouter>,
  );
  // Wait for initial loading to finish before clicking the section toggle.
  await waitFor(() => expect(screen.getByText('settings.copilotSettings')).toBeInTheDocument());
  fireEvent.click(screen.getByText('settings.copilotSettings'));
  await waitFor(() => expect(agentApi.get).toHaveBeenCalledWith('copilot'));
  // Now the save button should be present.
  await waitFor(() => expect(screen.getByText('settings.saveCopilot')).toBeInTheDocument());
}

async function clickSave() {
  fireEvent.click(screen.getByText('settings.saveCopilot'));
  await waitFor(() => expect(agentApi.update).toHaveBeenCalled());
}

function lastUpdatePayload(): UpdateAgentRequest {
  const calls = asMock(agentApi.update).mock.calls;
  expect(calls.length).toBeGreaterThan(0);
  return calls[calls.length - 1][1] as UpdateAgentRequest;
}

describe('SettingsPage CoPilot notification payload', () => {
  it('persists only {enabled: false} when notifications are disabled at load time', async () => {
    await expandCopilotSection(
      makeCopilotAgent({ enabled: false, channel_name: 'slack', granularity: 'all' }),
    );
    await clickSave();
    expect(lastUpdatePayload().config?.notification).toEqual({ enabled: false });
  });

  it('persists full notification payload when enabled', async () => {
    await expandCopilotSection(
      makeCopilotAgent({ enabled: true, channel_name: 'slack', granularity: 'all' }),
    );
    await clickSave();
    expect(lastUpdatePayload().config?.notification).toEqual({
      enabled: true,
      channel_name: 'slack',
      granularity: 'all',
    });
  });

  it('toggling enabled off and saving strips channel_name', async () => {
    await expandCopilotSection(
      makeCopilotAgent({ enabled: true, channel_name: 'slack', granularity: 'final' }),
    );

    const checkbox = screen.getByLabelText('settings.enableNotifications') as HTMLInputElement;
    expect(checkbox.checked).toBe(true);
    fireEvent.click(checkbox);

    await clickSave();
    expect(lastUpdatePayload().config?.notification).toEqual({ enabled: false });
  });
});
