import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, fireEvent, waitFor } from '@testing-library/react';
import PluginsPage from './PluginsPage';
import { pluginApi } from '../utils/api';
import type { Plugin } from '../utils/api';

const { addToast } = vi.hoisted(() => ({ addToast: vi.fn() }));

vi.mock('../store/toastStore', () => {
  const state = { addToast };
  const useToastStore = (selector?: (s: typeof state) => unknown) =>
    selector ? selector(state) : state;
  (useToastStore as unknown as { getState: () => typeof state }).getState = () => state;
  return { useToastStore };
});

// Stable t fn matches react-i18next's real behaviour (same instance across
// renders for a given namespace). Without this, callbacks that capture `t`
// would change identity every render and could mask false-positive effect
// re-runs in tests.
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
vi.mock('react-i18next', () => ({
  useTranslation: () => stableI18n,
}));

const builtin: Plugin = {
  info: { id: 'core', name: 'Core', builtin: true, version: '1.0.0', type: 'tool' },
  status: 'active',
};
const externalActive: Plugin = {
  info: {
    id: 'echo',
    name: 'Echo',
    builtin: false,
    version: '0.0.0',
    type: 'tool',
    description: 'Echo plugin',
  },
  status: 'active',
};
const externalInactive: Plugin = {
  info: { id: 'broken', name: 'Broken', builtin: false, version: '2.0.0' },
  status: 'error',
  error: 'init failed',
};

type Mocked = ReturnType<typeof vi.fn>;
const asMock = (fn: unknown) => fn as unknown as Mocked;

// Provide stable defaults for every pluginApi method so individual tests
// only need to override the calls they care about. Without this, vitest's
// auto-restore would let real fetch calls escape into jsdom and hang.
beforeEach(() => {
  addToast.mockReset();
  vi.spyOn(pluginApi, 'list').mockResolvedValue([]);
  vi.spyOn(pluginApi, 'enable').mockResolvedValue(undefined);
  vi.spyOn(pluginApi, 'disable').mockResolvedValue(undefined);
  vi.spyOn(pluginApi, 'remove').mockResolvedValue(undefined);
  vi.spyOn(pluginApi, 'reload').mockResolvedValue({ added: [], removed: [] });
  vi.spyOn(pluginApi, 'upload').mockResolvedValue({});
});

afterEach(() => {
  vi.restoreAllMocks();
});

async function renderPage(plugins: Plugin[]) {
  asMock(pluginApi.list).mockResolvedValue(plugins);
  render(<PluginsPage />);
  await waitFor(() => expect(screen.getByText('plugins.title')).toBeInTheDocument());
}

describe('PluginsPage', () => {
  it('shows empty state when no plugins', async () => {
    await renderPage([]);
    expect(screen.getByText('plugins.noPlugins')).toBeInTheDocument();
  });

  it('renders nested info from /plugins response', async () => {
    await renderPage([externalActive, builtin]);
    expect(screen.getByText('Echo')).toBeInTheDocument();
    expect(screen.getByText('Core')).toBeInTheDocument();
    expect(screen.getByText('Echo plugin')).toBeInTheDocument();
    // version 0.0.0 should be hidden, 1.0.0 should be shown.
    expect(screen.getByText('plugins.version(version=1.0.0)')).toBeInTheDocument();
    expect(screen.queryByText('plugins.version(version=0.0.0)')).not.toBeInTheDocument();
  });

  it('shows plugin error inline', async () => {
    await renderPage([externalInactive]);
    expect(screen.getByText('init failed')).toBeInTheDocument();
  });

  it('builtin toggle is disabled', async () => {
    await renderPage([builtin]);
    const powerBtn = screen.getByTitle('plugins.builtinLocked');
    expect(powerBtn).toBeDisabled();
    fireEvent.click(powerBtn);
    expect(pluginApi.enable).not.toHaveBeenCalled();
    expect(pluginApi.disable).not.toHaveBeenCalled();
  });

  it('builtin row hides delete button', async () => {
    await renderPage([builtin]);
    expect(screen.queryByTitle('plugins.delete')).not.toBeInTheDocument();
  });

  it('clicking power on active external plugin calls disable + reload', async () => {
    await renderPage([externalActive]);
    const callsBefore = asMock(pluginApi.list).mock.calls.length;

    fireEvent.click(screen.getByTitle('plugins.disable'));
    await waitFor(() => expect(addToast).toHaveBeenCalledWith('success', 'plugins.disabled'));

    expect(pluginApi.disable).toHaveBeenCalledWith('echo');
    expect(asMock(pluginApi.list).mock.calls.length).toBeGreaterThan(callsBefore);
  });

  it('toggle failure surfaces a toast', async () => {
    asMock(pluginApi.disable).mockRejectedValueOnce(new Error('boom'));
    await renderPage([externalActive]);

    fireEvent.click(screen.getByTitle('plugins.disable'));
    await waitFor(() => expect(addToast).toHaveBeenCalledWith('error', 'boom'));
  });

  it('delete requires two clicks (confirm pattern)', async () => {
    await renderPage([externalActive]);

    fireEvent.click(screen.getByTitle('plugins.delete'));
    expect(pluginApi.remove).not.toHaveBeenCalled();
    expect(screen.getByTitle('plugins.deleteConfirm')).toBeInTheDocument();

    fireEvent.click(screen.getByTitle('plugins.deleteConfirm'));
    await waitFor(() => expect(addToast).toHaveBeenCalledWith('success', 'plugins.deleted'));
    expect(pluginApi.remove).toHaveBeenCalledWith('echo');
  });

  it('reload toast reports added/removed counts (not raw arrays)', async () => {
    asMock(pluginApi.reload).mockResolvedValueOnce({ added: ['x', 'y'], removed: ['z'] });
    await renderPage([externalActive]);

    fireEvent.click(screen.getByText('plugins.reload'));
    await waitFor(() =>
      expect(addToast).toHaveBeenCalledWith('success', expect.stringContaining('added=2')),
    );
    expect(addToast).toHaveBeenCalledWith('success', expect.stringContaining('removed=1'));
  });

  it('reload error shows toast', async () => {
    asMock(pluginApi.reload).mockRejectedValueOnce(new Error('nope'));
    await renderPage([externalActive]);

    fireEvent.click(screen.getByText('plugins.reload'));
    await waitFor(() => expect(addToast).toHaveBeenCalledWith('error', 'nope'));
  });

  it('filter to builtin hides external plugins', async () => {
    await renderPage([externalActive, builtin]);
    fireEvent.click(screen.getByText('plugins.builtin'));
    expect(screen.getByText('Core')).toBeInTheDocument();
    expect(screen.queryByText('Echo')).not.toBeInTheDocument();
  });

  it('filter to external hides builtins', async () => {
    await renderPage([externalActive, builtin]);
    fireEvent.click(screen.getByText('plugins.external'));
    expect(screen.getByText('Echo')).toBeInTheDocument();
    expect(screen.queryByText('Core')).not.toBeInTheDocument();
  });

  it('upload posts the file and reloads on success', async () => {
    await renderPage([]);
    const file = new File(['payload'], 'echo', { type: 'application/octet-stream' });
    const input = document.querySelector('input[type=file]') as HTMLInputElement;

    fireEvent.change(input, { target: { files: [file] } });
    await waitFor(() => expect(addToast).toHaveBeenCalledWith('success', 'plugins.uploadSuccess'));

    expect(pluginApi.upload).toHaveBeenCalledWith(file);
    expect(input.value).toBe('');
  });

  it('upload error surfaces toast', async () => {
    asMock(pluginApi.upload).mockRejectedValueOnce(new Error('upload broke'));
    await renderPage([]);

    const file = new File(['payload'], 'echo');
    const input = document.querySelector('input[type=file]') as HTMLInputElement;
    fireEvent.change(input, { target: { files: [file] } });

    await waitFor(() => expect(addToast).toHaveBeenCalledWith('error', 'upload broke'));
  });

  it('initial load failure shows toast', async () => {
    // Regression: the page used to swallow load errors silently. After the
    // fix it should surface them via toast.
    asMock(pluginApi.list).mockRejectedValueOnce(new Error('cannot reach API'));
    render(<PluginsPage />);
    await waitFor(() => expect(addToast).toHaveBeenCalledWith('error', 'cannot reach API'));
  });
});
