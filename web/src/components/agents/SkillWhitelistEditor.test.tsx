import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, fireEvent, waitFor } from '@testing-library/react';
import SkillWhitelistEditor from './SkillWhitelistEditor';
import { skillApi, type SkillItem } from '../../utils/api';

const { addToast } = vi.hoisted(() => ({ addToast: vi.fn() }));

vi.mock('../../store/toastStore', () => {
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

const skillA: SkillItem = { name: 'analyzer', description: 'Analyzer', builtin: true, enabled: true };
const skillB: SkillItem = { name: 'web-search', description: 'Search', builtin: false, enabled: true };
const skillDisabled: SkillItem = { name: 'broken', description: 'Off', builtin: false, enabled: false };

type Mocked = ReturnType<typeof vi.fn>;
const asMock = (fn: unknown) => fn as unknown as Mocked;

beforeEach(() => {
  addToast.mockReset();
  vi.spyOn(skillApi, 'list').mockResolvedValue([skillA, skillB]);
});

afterEach(() => {
  vi.restoreAllMocks();
});

async function renderEditor(whitelist: string[], onChange = vi.fn()) {
  render(<SkillWhitelistEditor whitelist={whitelist} onChange={onChange} />);
  await waitFor(() => expect(screen.getByText('analyzer')).toBeInTheDocument());
  return { onChange };
}

describe('SkillWhitelistEditor', () => {
  it('renders all installed skills with a checkbox', async () => {
    await renderEditor([]);
    expect(screen.getByText('analyzer')).toBeInTheDocument();
    expect(screen.getByText('web-search')).toBeInTheDocument();
    expect(screen.getAllByRole('checkbox')).toHaveLength(2);
  });

  it('shows allAllowed hint when whitelist is empty', async () => {
    await renderEditor([]);
    expect(screen.getByText('skillWhitelist.allAllowed')).toBeInTheDocument();
  });

  it('shows count when whitelist has entries', async () => {
    await renderEditor(['analyzer']);
    expect(screen.getByText('skillWhitelist.selected(count=1)')).toBeInTheDocument();
  });

  it('shows builtin badge', async () => {
    await renderEditor([]);
    expect(screen.getByText('skills.builtin')).toBeInTheDocument();
  });

  it('shows disabled badge when skill.enabled is false', async () => {
    asMock(skillApi.list).mockResolvedValue([skillDisabled]);
    render(<SkillWhitelistEditor whitelist={[]} onChange={vi.fn()} />);
    await waitFor(() => expect(screen.getByText('broken')).toBeInTheDocument());
    expect(screen.getByText('skills.disabled')).toBeInTheDocument();
  });

  it('toggling unchecked checkbox emits a new whitelist with the skill added', async () => {
    const { onChange } = await renderEditor([]);
    const checkboxes = screen.getAllByRole('checkbox');
    fireEvent.click(checkboxes[0]);
    expect(onChange).toHaveBeenCalledWith(['analyzer']);
  });

  it('toggling a checked skill removes it from the whitelist', async () => {
    const { onChange } = await renderEditor(['analyzer', 'web-search']);
    const checkboxes = screen.getAllByRole('checkbox');
    fireEvent.click(checkboxes[0]);
    expect(onChange).toHaveBeenCalledWith(['web-search']);
  });

  it('deduplicates when a skill name is already present', async () => {
    const { onChange } = await renderEditor(['web-search']);
    // analyzer is currently unchecked; clicking should add it once even if state would otherwise duplicate.
    const checkboxes = screen.getAllByRole('checkbox');
    fireEvent.click(checkboxes[0]);
    expect(onChange).toHaveBeenCalledWith(['web-search', 'analyzer']);
  });

  it('renders ghost entry for whitelist names not in installed list', async () => {
    await renderEditor(['analyzer', 'gone-skill']);
    // ghost entry visible with missing label
    expect(screen.getByText('gone-skill')).toBeInTheDocument();
    expect(screen.getByText('skillWhitelist.missing')).toBeInTheDocument();
  });

  it('clicking ghost checkbox prunes the stale name', async () => {
    const { onChange } = await renderEditor(['analyzer', 'gone-skill']);
    // Ghost is the third checkbox (after the 2 installed ones)
    const checkboxes = screen.getAllByRole('checkbox');
    expect(checkboxes).toHaveLength(3);
    fireEvent.click(checkboxes[2]);
    expect(onChange).toHaveBeenCalledWith(['analyzer']);
  });

  it('shows empty message when no installed skills and no ghosts', async () => {
    asMock(skillApi.list).mockResolvedValue([]);
    render(<SkillWhitelistEditor whitelist={[]} onChange={vi.fn()} />);
    await waitFor(() => expect(screen.getByText('skillWhitelist.empty')).toBeInTheDocument());
  });

  it('still renders ghosts even when no installed skills exist', async () => {
    asMock(skillApi.list).mockResolvedValue([]);
    render(<SkillWhitelistEditor whitelist={['phantom']} onChange={vi.fn()} />);
    await waitFor(() => expect(screen.getByText('phantom')).toBeInTheDocument());
    expect(screen.queryByText('skillWhitelist.empty')).not.toBeInTheDocument();
  });

  it('list failure surfaces toast (no longer silently swallowed)', async () => {
    asMock(skillApi.list).mockRejectedValueOnce(new Error('network down'));
    render(<SkillWhitelistEditor whitelist={[]} onChange={vi.fn()} />);
    await waitFor(() => expect(addToast).toHaveBeenCalledWith('error', 'network down'));
  });

  it('skips skills with missing name field defensively', async () => {
    asMock(skillApi.list).mockResolvedValue([{ description: 'no name' } as SkillItem, skillA]);
    render(<SkillWhitelistEditor whitelist={[]} onChange={vi.fn()} />);
    await waitFor(() => expect(screen.getByText('analyzer')).toBeInTheDocument());
    // Only one checkbox rendered (the one with a real name)
    expect(screen.getAllByRole('checkbox')).toHaveLength(1);
  });
});
