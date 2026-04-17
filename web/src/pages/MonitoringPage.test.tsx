import { describe, it, expect, vi, beforeEach } from 'vitest';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import MonitoringPage from './MonitoringPage';

const navigateMock = vi.fn();
const summaryMock = vi.fn();
const timeseriesMock = vi.fn();
const runtimeMock = vi.fn();
const diagnosticsMock = vi.fn();
const agentListMock = vi.fn();

vi.mock('react-router-dom', async () => {
  const actual = await vi.importActual<typeof import('react-router-dom')>('react-router-dom');
  return {
    ...actual,
    useNavigate: () => navigateMock,
  };
});

vi.mock('react-i18next', () => ({
  useTranslation: () => ({
    t: (k: string) => k,
  }),
}));

vi.mock('../utils/api', () => ({
  monitoringApi: {
    summary: (...args: unknown[]) => summaryMock(...args),
    timeseries: (...args: unknown[]) => timeseriesMock(...args),
    runtime: (...args: unknown[]) => runtimeMock(...args),
    diagnostics: (...args: unknown[]) => diagnosticsMock(...args),
  },
  agentApi: {
    list: (...args: unknown[]) => agentListMock(...args),
  },
}));

describe('MonitoringPage', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    agentListMock.mockResolvedValue([{ id: 'a1', name: 'Agent A' }]);
    summaryMock.mockResolvedValue({
      run_total: 2,
      run_success: 1,
      run_failed: 1,
      success_rate: 0.5,
      error_rate: 0.5,
      health: 'degraded',
      active_actors: 1,
      active_sandboxes: 1,
      thresholds: {
        error_rate_warn: 0.05,
        error_rate_down: 0.2,
        latency_p95_warn_ms: 3000,
        consecutive_buckets: 3,
        no_success_down_minutes: 2,
      },
    });
    timeseriesMock.mockResolvedValue([
      { bucket_start: new Date().toISOString(), run_total: 1, run_success: 0, run_failed: 1, error_rate: 1, throughput_rpm: 1 },
    ]);
    runtimeMock.mockResolvedValue({ runtime_count: 1, actor_count: 1, current: { runtime_id: 'r', actor_count: 1, kanban_card_count: 1, sandbox_leases: 1 } });
    diagnosticsMock.mockResolvedValue({
      top_failed_agents: [{ agent_id: 'a1', failed_runs: 1, total_runs: 1 }],
      top_error_codes: [{ code: 'timeout', count: 1 }],
      recent_failures: [{ run_id: 'run1', agent_id: 'a1', error_code: 'timeout', message: 'x', elapsed_ms: 1, created_at: new Date().toISOString() }],
    });
  });

  it('navigates to run detail when clicking failure row', async () => {
    render(<MonitoringPage />);
    await waitFor(() => expect(screen.getByText('run1')).toBeInTheDocument());
    fireEvent.click(screen.getByText('run1'));
    expect(navigateMock).toHaveBeenCalledWith('/agents/a1/runs/run1');
  });

  it('shows partial error and avoids zero fallback when summary fails', async () => {
    summaryMock.mockRejectedValue(new Error('boom'));
    render(<MonitoringPage />);
    await waitFor(() => expect(screen.getByText(/monitoringV2.summaryLoadFailed/)).toBeInTheDocument());
    expect(screen.getAllByText('--').length).toBeGreaterThan(0);
  });

  it('re-fetches with agent filter parameter', async () => {
    render(<MonitoringPage />);
    await waitFor(() => expect(screen.getByText('Agent A')).toBeInTheDocument());
    const selects = screen.getAllByRole('combobox');
    fireEvent.change(selects[2], { target: { value: 'a1' } });
    await waitFor(() => {
      expect(summaryMock).toHaveBeenCalledWith(expect.objectContaining({ agentId: 'a1' }));
    });
  });
});

