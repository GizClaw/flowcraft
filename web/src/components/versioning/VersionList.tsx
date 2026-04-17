import { useState, useEffect } from 'react';
import { useTranslation } from 'react-i18next';
import { RotateCcw, GitBranch } from 'lucide-react';
import { versionApi, type GraphVersion } from '../../utils/api';
import { useToastStore } from '../../store/toastStore';
import { formatDistanceToNow } from 'date-fns';
import PublishDialog from './PublishDialog';

interface Props {
  agentId: string;
  onSelectDiff?: (v1: number, v2: number) => void;
}

export default function VersionList({ agentId, onSelectDiff }: Props) {
  const { t } = useTranslation();
  const [versions, setVersions] = useState<GraphVersion[]>([]);
  const [, setLoading] = useState(false);
  const addToast = useToastStore((s) => s.addToast);

  const loadVersions = () => {
    setLoading(true);
    versionApi.list(agentId).then(setVersions).catch(() => {}).finally(() => setLoading(false));
  };

  useEffect(loadVersions, [agentId]);

  const handleRollback = async (version: number) => {
    try {
      await versionApi.rollback(agentId, version);
      addToast('success', t('versions.rolledBack', { version }));
      loadVersions();
    } catch (err) {
      addToast('error', err instanceof Error ? err.message : t('versions.rollbackFailed'));
    }
  };

  const isPublished = (v: GraphVersion) => v.published_at != null;
  const draftVersion = versions.find((v) => !isPublished(v));
  const latestPublished = versions.find((v) => isPublished(v));

  return (
    <div className="space-y-4">
      <div className="flex items-center justify-between">
        <h3 className="text-sm font-semibold text-gray-700 dark:text-gray-300">{t('versions.title')}</h3>
        <PublishDialog agentId={agentId} currentVersion={draftVersion} latestPublishedVersion={latestPublished} onPublished={loadVersions} />
      </div>
      <div className="space-y-2">
        {versions.map((v, i) => {
          const published = isPublished(v);
          return (
            <div key={v.version} className="flex items-center gap-3 p-3 bg-gray-50 dark:bg-gray-800 rounded-lg">
              <GitBranch size={16} className={published ? 'text-green-500' : 'text-gray-400'} />
              <div className="flex-1">
                <div className="flex items-center gap-2">
                  <span className="text-sm font-medium text-gray-800 dark:text-gray-200">v{v.version}</span>
                  <span className={`text-[10px] px-1.5 py-0.5 rounded-full ${published ? 'bg-green-100 dark:bg-green-900 text-green-700 dark:text-green-300' : 'bg-gray-200 dark:bg-gray-700 text-gray-600 dark:text-gray-400'}`}>
                    {published ? t('versions.published') : t('versions.draft')}
                  </span>
                </div>
                {v.description && <p className="text-xs text-gray-500 mt-0.5">{v.description}</p>}
                <p className="text-xs text-gray-400">{formatDistanceToNow(new Date(v.created_at), { addSuffix: true })}</p>
              </div>
              <div className="flex gap-1">
                {i > 0 && onSelectDiff && (
                  <button onClick={() => onSelectDiff(v.version, versions[i - 1].version)} className="text-xs px-2 py-1 rounded border border-gray-300 dark:border-gray-600 hover:bg-gray-100 dark:hover:bg-gray-700">
                    {t('versions.diff')}
                  </button>
                )}
                {published && (
                  <button onClick={() => handleRollback(v.version)} className="text-xs px-2 py-1 rounded border border-amber-300 dark:border-amber-700 text-amber-600 hover:bg-amber-50 dark:hover:bg-amber-950">
                    <RotateCcw size={12} />
                  </button>
                )}
              </div>
            </div>
          );
        })}
      </div>
    </div>
  );
}
