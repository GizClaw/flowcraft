import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, fireEvent, waitFor, within } from '@testing-library/react';
import SkillsPage from './SkillsPage';
import { skillApi, type SkillItem } from '../utils/api';

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

const builtin: SkillItem = {
  name: 'analyzer',
  description: 'Built-in analyzer',
  builtin: true,
  enabled: true,
};
const externalGit: SkillItem = {
  name: 'web-search',
  description: 'External skill from git',
  builtin: false,
  enabled: true,
  source: 'https://github.com/foo/bar.git',
  tags: ['search', 'tools'],
};
const externalNoSource: SkillItem = {
  name: 'local-skill',
  description: 'Installed without git lockfile',
  builtin: false,
  enabled: true,
};
const disabledSkill: SkillItem = {
  name: 'broken',
  description: 'Disabled by config',
  builtin: false,
  enabled: false,
};

type Mocked = ReturnType<typeof vi.fn>;
const asMock = (fn: unknown) => fn as unknown as Mocked;

beforeEach(() => {
  addToast.mockReset();
  vi.spyOn(skillApi, 'list').mockResolvedValue([]);
  vi.spyOn(skillApi, 'install').mockResolvedValue({} as never);
  vi.spyOn(skillApi, 'uninstall').mockResolvedValue(undefined);
  vi.spyOn(skillApi, 'update').mockResolvedValue({} as never);
  vi.spyOn(skillApi, 'updateAll').mockResolvedValue({ updated: [] } as never);
});

afterEach(() => {
  vi.restoreAllMocks();
});

async function renderPage(skills: SkillItem[]) {
  asMock(skillApi.list).mockResolvedValue(skills);
  render(<SkillsPage />);
  await waitFor(() => expect(screen.getByText('skills.title')).toBeInTheDocument());
}

describe('SkillsPage', () => {
  it('shows empty state when no skills installed', async () => {
    await renderPage([]);
    expect(screen.getByText('skills.noSkills')).toBeInTheDocument();
    expect(screen.queryByText('skills.updateAll')).not.toBeInTheDocument();
  });

  it('renders skill cards with builtin / source / tags', async () => {
    await renderPage([builtin, externalGit]);
    expect(screen.getByText('analyzer')).toBeInTheDocument();
    expect(screen.getByText('web-search')).toBeInTheDocument();
    expect(screen.getByText('skills.builtin')).toBeInTheDocument();
    expect(screen.getByText('search')).toBeInTheDocument();
    expect(screen.getByText('tools')).toBeInTheDocument();
    expect(screen.getByText(/skills\.source/)).toBeInTheDocument();
  });

  it('renders disabled badge when enabled=false', async () => {
    await renderPage([disabledSkill]);
    expect(screen.getByText('skills.disabled')).toBeInTheDocument();
  });

  it('shows Update All only when at least one git-installed skill exists', async () => {
    await renderPage([builtin, externalNoSource]);
    expect(screen.queryByText('skills.updateAll')).not.toBeInTheDocument();
  });

  it('Update All visible when skill has source', async () => {
    await renderPage([externalGit]);
    expect(screen.getByText('skills.updateAll')).toBeInTheDocument();
  });

  it('hides update icon for non-git skills', async () => {
    await renderPage([externalNoSource]);
    expect(screen.queryByTitle('skills.update')).not.toBeInTheDocument();
  });

  it('hides uninstall icon for builtin skills', async () => {
    await renderPage([builtin]);
    expect(screen.queryByTitle('skills.uninstall')).not.toBeInTheDocument();
  });

  it('initial load failure shows toast (no longer silent)', async () => {
    asMock(skillApi.list).mockRejectedValueOnce(new Error('cannot reach API'));
    render(<SkillsPage />);
    await waitFor(() => expect(addToast).toHaveBeenCalledWith('error', 'cannot reach API'));
  });

  describe('install', () => {
    it('opens modal and submits trimmed url + custom name', async () => {
      await renderPage([]);
      fireEvent.click(screen.getByText('skills.installSkill'));

      const urlInput = screen.getByPlaceholderText('skills.gitUrl') as HTMLInputElement;
      const nameInput = screen.getByPlaceholderText('skills.customName') as HTMLInputElement;
      fireEvent.change(urlInput, { target: { value: '  https://github.com/foo/bar.git  ' } });
      fireEvent.change(nameInput, { target: { value: '  my-skill  ' } });

      fireEvent.click(screen.getByText('skills.install'));
      await waitFor(() =>
        expect(skillApi.install).toHaveBeenCalledWith({
          url: 'https://github.com/foo/bar.git',
          name: 'my-skill',
        }),
      );
    });

    it('omits name when input is blank', async () => {
      await renderPage([]);
      fireEvent.click(screen.getByText('skills.installSkill'));
      fireEvent.change(screen.getByPlaceholderText('skills.gitUrl'), {
        target: { value: 'git@github.com:foo/bar.git' },
      });
      fireEvent.click(screen.getByText('skills.install'));

      await waitFor(() => expect(skillApi.install).toHaveBeenCalledTimes(1));
      const arg = asMock(skillApi.install).mock.calls[0][0] as { name?: string };
      expect(arg).toEqual({ url: 'git@github.com:foo/bar.git' });
      expect(arg).not.toHaveProperty('name');
    });

    it('blocks invalid git URL with inline error and does not call API', async () => {
      await renderPage([]);
      fireEvent.click(screen.getByText('skills.installSkill'));
      fireEvent.change(screen.getByPlaceholderText('skills.gitUrl'), {
        target: { value: 'not-a-url' },
      });

      // Inline error is rendered immediately
      expect(screen.getByText('skills.invalidUrl')).toBeInTheDocument();
      // Submit button is disabled
      expect(screen.getByText('skills.install').closest('button')).toBeDisabled();

      expect(skillApi.install).not.toHaveBeenCalled();
    });

    it('accepts ssh-style URLs (git@host:path)', async () => {
      await renderPage([]);
      fireEvent.click(screen.getByText('skills.installSkill'));
      fireEvent.change(screen.getByPlaceholderText('skills.gitUrl'), {
        target: { value: 'git@github.com:foo/bar.git' },
      });
      expect(screen.queryByText('skills.invalidUrl')).not.toBeInTheDocument();
      expect(screen.getByText('skills.install').closest('button')).not.toBeDisabled();
    });

    it('install failure surfaces backend error message', async () => {
      asMock(skillApi.install).mockRejectedValueOnce(new Error('clone refused'));
      await renderPage([]);
      fireEvent.click(screen.getByText('skills.installSkill'));
      fireEvent.change(screen.getByPlaceholderText('skills.gitUrl'), {
        target: { value: 'https://github.com/foo/bar.git' },
      });
      fireEvent.click(screen.getByText('skills.install'));

      await waitFor(() => expect(addToast).toHaveBeenCalledWith('error', 'clone refused'));
    });

    it('install success closes modal, resets form, refreshes list', async () => {
      await renderPage([]);
      const callsBefore = asMock(skillApi.list).mock.calls.length;

      fireEvent.click(screen.getByText('skills.installSkill'));
      fireEvent.change(screen.getByPlaceholderText('skills.gitUrl'), {
        target: { value: 'https://github.com/foo/bar.git' },
      });
      fireEvent.change(screen.getByPlaceholderText('skills.customName'), {
        target: { value: 'foo' },
      });
      fireEvent.click(screen.getByText('skills.install'));

      await waitFor(() => expect(addToast).toHaveBeenCalledWith('success', 'skills.installed'));
      expect(asMock(skillApi.list).mock.calls.length).toBeGreaterThan(callsBefore);
      expect(screen.queryByPlaceholderText('skills.gitUrl')).not.toBeInTheDocument();
    });
  });

  describe('uninstall', () => {
    it('shows confirm dialog before deleting (no immediate API call)', async () => {
      await renderPage([externalGit]);
      fireEvent.click(screen.getByTitle('skills.uninstall'));

      expect(screen.getByText('skills.uninstallConfirmTitle')).toBeInTheDocument();
      expect(skillApi.uninstall).not.toHaveBeenCalled();
    });

    it('confirming triggers uninstall and toasts success', async () => {
      await renderPage([externalGit]);
      fireEvent.click(screen.getByTitle('skills.uninstall'));

      // Click the confirm button inside the dialog (matches confirmLabel="skills.uninstall").
      const dialog = screen.getByText('skills.uninstallConfirmTitle').closest('div')!;
      const confirmBtn = within(dialog.parentElement as HTMLElement)
        .getAllByText('skills.uninstall')
        .find((el) => el.tagName === 'BUTTON') as HTMLButtonElement;
      fireEvent.click(confirmBtn);

      await waitFor(() => expect(skillApi.uninstall).toHaveBeenCalledWith('web-search'));
      await waitFor(() =>
        expect(addToast).toHaveBeenCalledWith('success', 'skills.uninstalled(name=web-search)'),
      );
    });

    it('uninstall failure surfaces toast', async () => {
      asMock(skillApi.uninstall).mockRejectedValueOnce(new Error('locked'));
      await renderPage([externalGit]);
      fireEvent.click(screen.getByTitle('skills.uninstall'));

      const dialog = screen.getByText('skills.uninstallConfirmTitle').closest('div')!;
      const confirmBtn = within(dialog.parentElement as HTMLElement)
        .getAllByText('skills.uninstall')
        .find((el) => el.tagName === 'BUTTON') as HTMLButtonElement;
      fireEvent.click(confirmBtn);

      await waitFor(() => expect(addToast).toHaveBeenCalledWith('error', 'locked'));
    });
  });

  describe('update', () => {
    it('per-skill update calls API and reloads', async () => {
      await renderPage([externalGit]);
      const callsBefore = asMock(skillApi.list).mock.calls.length;

      fireEvent.click(screen.getByTitle('skills.update'));
      await waitFor(() => expect(skillApi.update).toHaveBeenCalledWith('web-search'));
      await waitFor(() =>
        expect(addToast).toHaveBeenCalledWith('success', 'skills.updated(name=web-search)'),
      );
      expect(asMock(skillApi.list).mock.calls.length).toBeGreaterThan(callsBefore);
    });

    it('per-skill update failure shows toast', async () => {
      asMock(skillApi.update).mockRejectedValueOnce(new Error('pull failed'));
      await renderPage([externalGit]);
      fireEvent.click(screen.getByTitle('skills.update'));
      await waitFor(() => expect(addToast).toHaveBeenCalledWith('error', 'pull failed'));
    });

    it('Update All success reports updated count', async () => {
      asMock(skillApi.updateAll).mockResolvedValueOnce({ updated: ['a', 'b'] });
      await renderPage([externalGit]);
      fireEvent.click(screen.getByText('skills.updateAll'));

      await waitFor(() =>
        expect(addToast).toHaveBeenCalledWith('success', 'skills.updateAllSuccess(count=2)'),
      );
    });

    it('Update All with empty result shows info noop', async () => {
      asMock(skillApi.updateAll).mockResolvedValueOnce({ updated: [] });
      await renderPage([externalGit]);
      fireEvent.click(screen.getByText('skills.updateAll'));

      await waitFor(() => expect(addToast).toHaveBeenCalledWith('info', 'skills.updateAllNoop'));
    });

    it('Update All failure shows error toast', async () => {
      asMock(skillApi.updateAll).mockRejectedValueOnce(new Error('boom'));
      await renderPage([externalGit]);
      fireEvent.click(screen.getByText('skills.updateAll'));
      await waitFor(() => expect(addToast).toHaveBeenCalledWith('error', 'boom'));
    });
  });
});
