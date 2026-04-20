import { useState, useEffect, useRef, useCallback } from 'react';
import { useTranslation } from 'react-i18next';
import { Plug, Power, Upload, RefreshCw, Trash2 } from 'lucide-react';
import { pluginApi } from '../utils/api';
import type { Plugin } from '../utils/api';
import EmptyState from '../components/common/EmptyState';
import LoadingSpinner from '../components/common/LoadingSpinner';
import { useToastStore } from '../store/toastStore';

type FilterValue = 'all' | 'builtin' | 'external';

export default function PluginsPage() {
  const { t } = useTranslation();
  const [plugins, setPlugins] = useState<Plugin[]>([]);
  const [loading, setLoading] = useState(true);
  const [filter, setFilter] = useState<FilterValue>('all');
  const [reloading, setReloading] = useState(false);
  const [uploading, setUploading] = useState(false);
  const [togglingId, setTogglingId] = useState<string | null>(null);
  const [confirmDelete, setConfirmDelete] = useState<string | null>(null);
  const fileInputRef = useRef<HTMLInputElement>(null);
  const confirmTimerRef = useRef<ReturnType<typeof setTimeout>>(undefined);
  const addToast = useToastStore((s) => s.addToast);

  // pluginApi calls always throw ApiError (Error subclass) via the
  // throwOnError middleware, so we can rely on err.message and skip a t()
  // fallback. Keeping `t` out of this useCallback's deps prevents the
  // initial-load effect from re-firing on every i18n re-render.
  const load = useCallback(async () => {
    setLoading(true);
    try {
      setPlugins(await pluginApi.list());
    } catch (err) {
      addToast('error', (err as Error).message);
    } finally {
      setLoading(false);
    }
  }, [addToast]);

  useEffect(() => {
    load();
  }, [load]);

  useEffect(() => () => clearTimeout(confirmTimerRef.current), []);

  const filtered = filter === 'all'
    ? plugins
    : plugins.filter((p) => (p.info.builtin ? 'builtin' : 'external') === filter);

  const filterLabels: Record<FilterValue, string> = {
    all: t('plugins.all'),
    builtin: t('plugins.builtin'),
    external: t('plugins.external'),
  };

  const togglePlugin = async (id: string, enable: boolean) => {
    setTogglingId(id);
    try {
      if (enable) await pluginApi.enable(id); else await pluginApi.disable(id);
      await load();
      addToast('success', enable ? t('plugins.enabled') : t('plugins.disabled'));
    } catch (err) {
      addToast('error', (err as Error).message);
    } finally {
      setTogglingId(null);
    }
  };

  const handleReload = async () => {
    setReloading(true);
    try {
      const { added, removed } = await pluginApi.reload();
      await load();
      addToast('success', t('plugins.reloaded', { added: added.length, removed: removed.length }));
    } catch (err) {
      addToast('error', (err as Error).message);
    } finally {
      setReloading(false);
    }
  };

  const handleUpload = async (e: React.ChangeEvent<HTMLInputElement>) => {
    const file = e.target.files?.[0];
    if (!file) return;
    setUploading(true);
    try {
      await pluginApi.upload(file);
      await load();
      addToast('success', t('plugins.uploadSuccess'));
    } catch (err) {
      addToast('error', (err as Error).message);
    } finally {
      setUploading(false);
      if (fileInputRef.current) fileInputRef.current.value = '';
    }
  };

  const handleDelete = async (id: string) => {
    if (confirmDelete !== id) {
      setConfirmDelete(id);
      clearTimeout(confirmTimerRef.current);
      confirmTimerRef.current = setTimeout(() => setConfirmDelete(null), 3000);
      return;
    }
    clearTimeout(confirmTimerRef.current);
    setConfirmDelete(null);
    try {
      await pluginApi.remove(id);
      await load();
      addToast('success', t('plugins.deleted'));
    } catch (err) {
      addToast('error', (err as Error).message);
    }
  };

  if (loading) return <div className="flex justify-center py-16"><LoadingSpinner /></div>;

  return (
    <div className="p-6">
      <div className="flex items-center justify-between mb-6">
        <h1 className="text-2xl font-bold text-gray-900 dark:text-gray-100">{t('plugins.title')}</h1>
        <div className="flex items-center gap-2">
          <div className="flex gap-1" role="tablist" aria-label={t('plugins.filter')}>
            {(['all', 'builtin', 'external'] as const).map((f) => (
              <button
                key={f}
                role="tab"
                aria-selected={filter === f}
                onClick={() => setFilter(f)}
                className={`px-3 py-1.5 text-sm rounded-lg transition-colors ${
                  filter === f
                    ? 'bg-indigo-100 dark:bg-indigo-900/40 text-indigo-700 dark:text-indigo-300'
                    : 'text-gray-500 dark:text-gray-400 hover:bg-gray-100 dark:hover:bg-gray-800'
                }`}
              >
                {filterLabels[f]}
              </button>
            ))}
          </div>
          <div className="w-px h-6 bg-gray-200 dark:bg-gray-700" />
          <button
            onClick={handleReload}
            disabled={reloading}
            className="flex items-center gap-1.5 px-3 py-1.5 text-sm rounded-lg text-gray-600 dark:text-gray-400 hover:bg-gray-100 dark:hover:bg-gray-800 disabled:opacity-50 disabled:cursor-not-allowed"
          >
            <RefreshCw size={14} className={reloading ? 'animate-spin' : ''} />
            {t('plugins.reload')}
          </button>
          <button
            onClick={() => fileInputRef.current?.click()}
            disabled={uploading}
            className="flex items-center gap-1.5 px-3 py-1.5 text-sm rounded-lg bg-indigo-600 text-white hover:bg-indigo-700 disabled:opacity-50 disabled:cursor-not-allowed"
          >
            {uploading ? <LoadingSpinner size={14} /> : <Upload size={14} />}
            {t('plugins.upload')}
          </button>
          <input ref={fileInputRef} type="file" className="hidden" onChange={handleUpload} />
        </div>
      </div>

      {filtered.length === 0 ? (
        <EmptyState title={t('plugins.noPlugins')} description={t('plugins.noPluginsDesc')} />
      ) : (
        <div className="space-y-3">
          {filtered.map((p) => {
            const isActive = p.status === 'active';
            const isConfirming = confirmDelete === p.info.id;
            const isToggling = togglingId === p.info.id;
            const showVersion = p.info.version && p.info.version !== '0.0.0';
            return (
              <div key={p.info.id} className="flex items-center gap-4 p-4 bg-white dark:bg-gray-900 rounded-xl border border-gray-200 dark:border-gray-800">
                <Plug size={20} className={isActive ? 'text-green-500' : 'text-gray-400'} />
                <div className="flex-1 min-w-0">
                  <div className="flex items-center gap-2">
                    <h3 className="text-sm font-semibold text-gray-900 dark:text-gray-100 truncate">{p.info.name}</h3>
                    {p.info.type && (
                      <span className="text-[10px] px-1.5 py-0.5 rounded bg-gray-100 dark:bg-gray-800 text-gray-500 shrink-0">{p.info.type}</span>
                    )}
                    {showVersion && (
                      <span className="text-[10px] text-gray-400 shrink-0">{t('plugins.version', { version: p.info.version })}</span>
                    )}
                  </div>
                  {p.info.description && <p className="text-xs text-gray-500 truncate">{p.info.description}</p>}
                  {p.error && <p className="text-xs text-red-500 mt-0.5">{p.error}</p>}
                </div>
                <div className="flex items-center gap-1 shrink-0">
                  {!p.info.builtin && (
                    <button
                      onClick={() => handleDelete(p.info.id)}
                      className={`flex items-center gap-1 px-2 py-1.5 rounded-lg text-xs transition-colors ${
                        isConfirming
                          ? 'bg-red-100 dark:bg-red-900/40 text-red-600 dark:text-red-400'
                          : 'text-gray-400 hover:bg-gray-100 dark:hover:bg-gray-800 hover:text-red-500'
                      }`}
                      title={isConfirming ? t('plugins.deleteConfirm') : t('plugins.delete')}
                    >
                      <Trash2 size={14} />
                      {isConfirming && <span>{t('plugins.deleteConfirm')}</span>}
                    </button>
                  )}
                  <button
                    onClick={() => togglePlugin(p.info.id, !isActive)}
                    disabled={p.info.builtin || isToggling}
                    title={
                      p.info.builtin
                        ? t('plugins.builtinLocked')
                        : isActive
                          ? t('plugins.disable')
                          : t('plugins.enable')
                    }
                    className={`p-2 rounded-lg transition-colors disabled:cursor-not-allowed ${
                      isActive
                        ? 'bg-green-100 dark:bg-green-900/40 text-green-600 dark:text-green-400'
                        : 'bg-gray-100 dark:bg-gray-800 text-gray-400 dark:text-gray-500'
                    } ${p.info.builtin ? 'opacity-50' : ''}`}
                  >
                    {isToggling ? <LoadingSpinner size={16} /> : <Power size={16} />}
                  </button>
                </div>
              </div>
            );
          })}
        </div>
      )}
    </div>
  );
}
