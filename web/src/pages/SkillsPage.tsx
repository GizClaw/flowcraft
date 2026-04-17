import { useState, useEffect } from 'react';
import { useTranslation } from 'react-i18next';
import { Plus, Puzzle, Trash2 } from 'lucide-react';
import { skillApi, type SkillItem } from '../utils/api';
import Modal from '../components/common/Modal';
import EmptyState from '../components/common/EmptyState';
import LoadingSpinner from '../components/common/LoadingSpinner';
import { useToastStore } from '../store/toastStore';

export default function SkillsPage() {
  const { t } = useTranslation();
  const [skills, setSkills] = useState<SkillItem[]>([]);
  const [loading, setLoading] = useState(true);
  const [showInstall, setShowInstall] = useState(false);
  const [gitUrl, setGitUrl] = useState('');
  const [installing, setInstalling] = useState(false);
  const addToast = useToastStore((s) => s.addToast);

  const load = () => { setLoading(true); skillApi.list().then(setSkills).catch(() => {}).finally(() => setLoading(false)); };
  useEffect(load, []);

  const handleInstall = async () => {
    if (!gitUrl.trim()) return;
    setInstalling(true);
    try {
      await skillApi.install({ url: gitUrl.trim() });
      setShowInstall(false); setGitUrl(''); load();
      addToast('success', t('skills.installed'));
    } catch (err) { addToast('error', err instanceof Error ? err.message : t('skills.installFailed')); }
    finally { setInstalling(false); }
  };

  const handleUninstall = async (name: string) => {
    try { await skillApi.uninstall(name); load(); addToast('success', t('skills.uninstalled', { name })); }
    catch (err) { addToast('error', err instanceof Error ? err.message : t('skills.uninstallFailed')); }
  };

  if (loading) return <div className="flex justify-center py-16"><LoadingSpinner /></div>;

  return (
    <div className="p-6">
      <div className="flex items-center justify-between mb-6">
        <h1 className="text-2xl font-bold text-gray-900 dark:text-gray-100">{t('skills.title')}</h1>
        <button onClick={() => setShowInstall(true)} className="flex items-center gap-2 px-4 py-2 bg-indigo-600 text-white rounded-lg hover:bg-indigo-700 text-sm">
          <Plus size={16} /> {t('skills.installSkill')}
        </button>
      </div>

      {skills.length === 0 ? (
        <EmptyState title={t('skills.noSkills')} description={t('skills.noSkillsDesc')} />
      ) : (
        <div className="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-3 gap-4">
          {skills.map((s) => (
            <div key={s.name} className="bg-white dark:bg-gray-900 rounded-xl border border-gray-200 dark:border-gray-800 p-4">
              <div className="flex items-start justify-between mb-2">
                <div className="flex items-center gap-2">
                  <Puzzle size={16} className="text-purple-500" />
                  <h3 className="text-sm font-semibold text-gray-900 dark:text-gray-100">{s.name}</h3>
                  {s.builtin && <span className="text-[10px] px-1.5 py-0.5 rounded bg-indigo-100 dark:bg-indigo-900/30 text-indigo-600 dark:text-indigo-400">{t('skills.builtin')}</span>}
                </div>
                {!s.builtin && <button onClick={() => handleUninstall(s.name)} className="p-1 text-red-400 hover:text-red-600"><Trash2 size={14} /></button>}
              </div>
              <p className="text-xs text-gray-500 mb-2">{s.description}</p>
              {s.tags && s.tags.length > 0 && (
                <div className="flex flex-wrap gap-1">
                  {s.tags.map((tag) => <span key={tag} className="text-[10px] px-1.5 py-0.5 rounded bg-gray-100 dark:bg-gray-800 text-gray-500">{tag}</span>)}
                </div>
              )}
            </div>
          ))}
        </div>
      )}

      <Modal open={showInstall} onClose={() => setShowInstall(false)} title={t('skills.installSkill')} size="sm">
        <div className="space-y-3">
          <input value={gitUrl} onChange={(e) => setGitUrl(e.target.value)} placeholder={t('skills.gitUrl')} className="w-full px-3 py-2 text-sm rounded-lg border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-800" autoFocus />
          <button onClick={handleInstall} disabled={!gitUrl.trim() || installing} className="w-full px-4 py-2 bg-indigo-600 text-white rounded-lg hover:bg-indigo-700 disabled:opacity-50 text-sm">
            {installing ? t('skills.installing') : t('skills.install')}
          </button>
        </div>
      </Modal>
    </div>
  );
}
