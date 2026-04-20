import { useCallback, useEffect, useMemo, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Plus, Puzzle, RefreshCw, Trash2 } from 'lucide-react';
import { skillApi, type SkillItem } from '../utils/api';
import Modal from '../components/common/Modal';
import ConfirmDialog from '../components/common/ConfirmDialog';
import EmptyState from '../components/common/EmptyState';
import LoadingSpinner from '../components/common/LoadingSpinner';
import { useToastStore } from '../store/toastStore';

// Loose match for Git URLs we accept: https://, http://, git://, ssh://,
// or scp-like (user@host:path). The backend ultimately validates by trying
// to clone, but a client-side guard prevents obvious typos from round-trip.
const GIT_URL_RE = /^(https?:\/\/|git:\/\/|ssh:\/\/|git@|[\w.-]+@[\w.-]+:)/i;

export default function SkillsPage() {
  const { t } = useTranslation();
  const [skills, setSkills] = useState<SkillItem[]>([]);
  const [loading, setLoading] = useState(true);
  const [showInstall, setShowInstall] = useState(false);
  const [gitUrl, setGitUrl] = useState('');
  const [customName, setCustomName] = useState('');
  const [installing, setInstalling] = useState(false);
  const [updatingAll, setUpdatingAll] = useState(false);
  const [updatingName, setUpdatingName] = useState<string | null>(null);
  const [pendingUninstall, setPendingUninstall] = useState<string | null>(null);
  const addToast = useToastStore((s) => s.addToast);

  // addToast is the only external dep; pulling t() into deps would cause the
  // initial-load effect to re-fire on every i18n re-render (matches the
  // pattern already used in PluginsPage).
  const load = useCallback(async () => {
    setLoading(true);
    try {
      setSkills(await skillApi.list());
    } catch (err) {
      addToast('error', err instanceof Error ? err.message : 'Failed to load skills');
    } finally {
      setLoading(false);
    }
  }, [addToast]);

  useEffect(() => { load(); }, [load]);

  const hasGitInstalls = useMemo(() => skills.some((s) => !s.builtin && s.source), [skills]);

  const trimmedUrl = gitUrl.trim();
  const trimmedName = customName.trim();
  const urlValid = GIT_URL_RE.test(trimmedUrl);

  const resetInstallForm = () => {
    setGitUrl('');
    setCustomName('');
  };

  const handleInstall = async () => {
    if (!trimmedUrl) return;
    if (!urlValid) {
      addToast('error', t('skills.invalidUrl'));
      return;
    }
    setInstalling(true);
    try {
      await skillApi.install({ url: trimmedUrl, ...(trimmedName ? { name: trimmedName } : {}) });
      setShowInstall(false);
      resetInstallForm();
      addToast('success', t('skills.installed'));
      await load();
    } catch (err) {
      addToast('error', err instanceof Error ? err.message : t('skills.installFailed'));
    } finally {
      setInstalling(false);
    }
  };

  const handleUninstall = async (name: string) => {
    try {
      await skillApi.uninstall(name);
      addToast('success', t('skills.uninstalled', { name }));
      await load();
    } catch (err) {
      addToast('error', err instanceof Error ? err.message : t('skills.uninstallFailed'));
    }
  };

  const handleUpdate = async (name: string) => {
    setUpdatingName(name);
    try {
      await skillApi.update(name);
      addToast('success', t('skills.updated', { name }));
      await load();
    } catch (err) {
      addToast('error', err instanceof Error ? err.message : t('skills.updateFailed'));
    } finally {
      setUpdatingName(null);
    }
  };

  const handleUpdateAll = async () => {
    setUpdatingAll(true);
    try {
      const result = await skillApi.updateAll();
      const updated = result?.updated ?? [];
      if (updated.length === 0) {
        addToast('info', t('skills.updateAllNoop'));
      } else {
        addToast('success', t('skills.updateAllSuccess', { count: updated.length }));
      }
      await load();
    } catch (err) {
      addToast('error', err instanceof Error ? err.message : t('skills.updateFailed'));
    } finally {
      setUpdatingAll(false);
    }
  };

  if (loading) return <div className="flex justify-center py-16"><LoadingSpinner /></div>;

  return (
    <div className="p-6">
      <div className="flex items-center justify-between mb-6">
        <h1 className="text-2xl font-bold text-gray-900 dark:text-gray-100">{t('skills.title')}</h1>
        <div className="flex items-center gap-2">
          {hasGitInstalls && (
            <button
              onClick={handleUpdateAll}
              disabled={updatingAll}
              className="flex items-center gap-1.5 px-3 py-1.5 text-sm rounded-lg text-gray-600 dark:text-gray-400 hover:bg-gray-100 dark:hover:bg-gray-800 disabled:opacity-50 disabled:cursor-not-allowed"
            >
              <RefreshCw size={14} className={updatingAll ? 'animate-spin' : ''} />
              {updatingAll ? t('skills.updating') : t('skills.updateAll')}
            </button>
          )}
          <button
            onClick={() => setShowInstall(true)}
            className="flex items-center gap-2 px-4 py-2 bg-indigo-600 text-white rounded-lg hover:bg-indigo-700 text-sm"
          >
            <Plus size={16} /> {t('skills.installSkill')}
          </button>
        </div>
      </div>

      {skills.length === 0 ? (
        <EmptyState title={t('skills.noSkills')} description={t('skills.noSkillsDesc')} />
      ) : (
        <div className="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-3 gap-4">
          {skills.map((s) => {
            const name = s.name ?? '';
            if (!name) return null;
            const updatable = !s.builtin && Boolean(s.source);
            const isUpdating = updatingName === name;
            return (
              <div
                key={name}
                className="bg-white dark:bg-gray-900 rounded-xl border border-gray-200 dark:border-gray-800 p-4 flex flex-col"
              >
                <div className="flex items-start justify-between mb-2 gap-2">
                  <div className="flex items-center gap-2 min-w-0 flex-wrap">
                    <Puzzle size={16} className="text-purple-500 shrink-0" />
                    <h3 className="text-sm font-semibold text-gray-900 dark:text-gray-100 truncate">{name}</h3>
                    {s.builtin && (
                      <span className="text-[10px] px-1.5 py-0.5 rounded bg-indigo-100 dark:bg-indigo-900/30 text-indigo-600 dark:text-indigo-400">
                        {t('skills.builtin')}
                      </span>
                    )}
                    {s.enabled === false && (
                      <span className="text-[10px] px-1.5 py-0.5 rounded bg-gray-200 dark:bg-gray-700 text-gray-500">
                        {t('skills.disabled')}
                      </span>
                    )}
                  </div>
                  <div className="flex items-center gap-1 shrink-0">
                    {updatable && (
                      <button
                        onClick={() => handleUpdate(name)}
                        disabled={isUpdating}
                        title={t('skills.update')}
                        className="p-1 text-gray-400 hover:text-indigo-600 disabled:opacity-50"
                      >
                        {isUpdating ? <LoadingSpinner size={14} /> : <RefreshCw size={14} />}
                      </button>
                    )}
                    {!s.builtin && (
                      <button
                        onClick={() => setPendingUninstall(name)}
                        title={t('skills.uninstall')}
                        className="p-1 text-red-400 hover:text-red-600"
                      >
                        <Trash2 size={14} />
                      </button>
                    )}
                  </div>
                </div>
                {s.description && <p className="text-xs text-gray-500 mb-2 line-clamp-3">{s.description}</p>}
                {s.tags && s.tags.length > 0 && (
                  <div className="flex flex-wrap gap-1 mb-2">
                    {s.tags.map((tag) => (
                      <span
                        key={tag}
                        className="text-[10px] px-1.5 py-0.5 rounded bg-gray-100 dark:bg-gray-800 text-gray-500"
                      >
                        {tag}
                      </span>
                    ))}
                  </div>
                )}
                {s.source && (
                  <p
                    className="text-[10px] text-gray-400 truncate mt-auto"
                    title={s.source}
                  >
                    {t('skills.source')}: {s.source}
                  </p>
                )}
              </div>
            );
          })}
        </div>
      )}

      <Modal
        open={showInstall}
        onClose={() => { setShowInstall(false); resetInstallForm(); }}
        title={t('skills.installSkill')}
        size="sm"
      >
        <div className="space-y-3">
          <div>
            <input
              value={gitUrl}
              onChange={(e) => setGitUrl(e.target.value)}
              placeholder={t('skills.gitUrl')}
              className="w-full px-3 py-2 text-sm rounded-lg border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-800"
              autoFocus
            />
            {trimmedUrl && !urlValid && (
              <p className="mt-1 text-[11px] text-red-500">{t('skills.invalidUrl')}</p>
            )}
          </div>
          <div>
            <input
              value={customName}
              onChange={(e) => setCustomName(e.target.value)}
              placeholder={t('skills.customName')}
              className="w-full px-3 py-2 text-sm rounded-lg border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-800"
            />
            <p className="mt-1 text-[11px] text-gray-400">{t('skills.customNameHint')}</p>
          </div>
          <button
            onClick={handleInstall}
            disabled={!trimmedUrl || !urlValid || installing}
            className="w-full px-4 py-2 bg-indigo-600 text-white rounded-lg hover:bg-indigo-700 disabled:opacity-50 text-sm flex items-center justify-center gap-2"
          >
            {installing && <LoadingSpinner size={14} className="text-white" />}
            {installing ? t('skills.installing') : t('skills.install')}
          </button>
        </div>
      </Modal>

      <ConfirmDialog
        open={pendingUninstall !== null}
        onClose={() => setPendingUninstall(null)}
        onConfirm={() => {
          if (pendingUninstall) handleUninstall(pendingUninstall);
        }}
        title={t('skills.uninstallConfirmTitle')}
        message={t('skills.uninstallConfirm', { name: pendingUninstall ?? '' })}
        confirmLabel={t('skills.uninstall')}
        variant="danger"
      />
    </div>
  );
}
