import { useState, useEffect } from 'react';
import { useOutletContext, useNavigate } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { formatDistanceToNow } from 'date-fns';
import { workflowRunApi } from '../utils/api';
import type { WorkflowRun } from '../types/chat';
import type { Agent } from '../types/app';
import LoadingSpinner from '../components/common/LoadingSpinner';
import EmptyState from '../components/common/EmptyState';

export default function AgentLogsPage() {
  const { t } = useTranslation();
  const { agent } = useOutletContext<{ agent: Agent }>();
  const navigate = useNavigate();
  const [runs, setRuns] = useState<WorkflowRun[]>([]);
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    setLoading(true);
    workflowRunApi.list(agent.id).then(setRuns).catch(() => {}).finally(() => setLoading(false));
  }, [agent.id]);

  const statusColor: Record<string, string> = {
    completed: 'bg-green-100 dark:bg-green-900 text-green-700 dark:text-green-300',
    failed: 'bg-red-100 dark:bg-red-900 text-red-700 dark:text-red-300',
    running: 'bg-blue-100 dark:bg-blue-900 text-blue-700 dark:text-blue-300',
    interrupted: 'bg-amber-100 dark:bg-amber-900 text-amber-700 dark:text-amber-300',
    timeout: 'bg-gray-100 dark:bg-gray-800 text-gray-600 dark:text-gray-400',
  };

  if (loading) return <div className="flex justify-center py-16"><LoadingSpinner /></div>;
  if (runs.length === 0) return <EmptyState title={t('logs.noRuns')} description={t('logs.noRunsDesc')} />;

  return (
    <div className="p-4">
      <div className="bg-white dark:bg-gray-900 rounded-xl border border-gray-200 dark:border-gray-800 overflow-hidden">
        <table className="w-full text-sm">
          <thead>
            <tr className="border-b border-gray-200 dark:border-gray-800">
              <th className="text-left px-4 py-3 text-xs font-medium text-gray-500 uppercase">{t('logs.runId')}</th>
              <th className="text-left px-4 py-3 text-xs font-medium text-gray-500 uppercase">Conversation</th>
              <th className="text-left px-4 py-3 text-xs font-medium text-gray-500 uppercase">{t('logs.status')}</th>
              <th className="text-left px-4 py-3 text-xs font-medium text-gray-500 uppercase">{t('logs.duration')}</th>
              <th className="text-left px-4 py-3 text-xs font-medium text-gray-500 uppercase">{t('logs.time')}</th>
            </tr>
          </thead>
          <tbody>
            {runs.map((run) => (
              <tr
                key={run.id}
                onClick={() => navigate(`../runs/${run.id}`)}
                className="border-b border-gray-100 dark:border-gray-800 hover:bg-gray-50 dark:hover:bg-gray-800 cursor-pointer"
              >
                <td className="px-4 py-3 font-mono text-xs text-gray-600 dark:text-gray-400">{run.id.slice(0, 12)}</td>
                <td className="px-4 py-3 text-xs text-gray-500 dark:text-gray-400">
                  {run.conversation_id ? (
                    <span className="font-mono bg-gray-100 dark:bg-gray-800 px-1.5 py-0.5 rounded">{run.conversation_id}</span>
                  ) : (
                    <span className="text-gray-300 dark:text-gray-600">—</span>
                  )}
                </td>
                <td className="px-4 py-3">
                  <span className={`text-[10px] px-2 py-0.5 rounded-full font-medium ${statusColor[run.status] || 'bg-gray-100 text-gray-600'}`}>{run.status}</span>
                </td>
                <td className="px-4 py-3 text-xs text-gray-500">{(run.elapsed_ms / 1000).toFixed(2)}s</td>
                <td className="px-4 py-3 text-xs text-gray-400">{formatDistanceToNow(new Date(run.created_at), { addSuffix: true })}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </div>
  );
}
