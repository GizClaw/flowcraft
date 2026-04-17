import { useState, useEffect } from 'react';
import { useParams } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { workflowRunApi } from '../utils/api';
import type { WorkflowRun, ExecutionEvent } from '../types/chat';
import EventTimeline from '../components/common/EventTimeline';
import LoadingSpinner from '../components/common/LoadingSpinner';

export default function AgentRunDetailPage() {
  const { t } = useTranslation();
  const { runId } = useParams<{ runId: string }>();
  const [run, setRun] = useState<WorkflowRun | null>(null);
  const [events, setEvents] = useState<ExecutionEvent[]>([]);
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    if (!runId) return;
    setLoading(true);
    Promise.all([workflowRunApi.get(runId), workflowRunApi.events(runId)])
      .then(([r, e]) => { setRun(r); setEvents(e); })
      .catch(() => {})
      .finally(() => setLoading(false));
  }, [runId]);

  if (loading) return <div className="flex justify-center py-16"><LoadingSpinner /></div>;
  if (!run) return <p className="text-center text-gray-500 py-8">{t('runDetail.notFound')}</p>;

  return (
    <div className="p-6 max-w-3xl mx-auto space-y-6">
      <div className="bg-white dark:bg-gray-900 rounded-xl border border-gray-200 dark:border-gray-800 p-4">
        <h2 className="text-lg font-semibold text-gray-900 dark:text-gray-100 mb-3">{t('runDetail.title')}</h2>
        <dl className="grid grid-cols-2 gap-2 text-sm">
          <dt className="text-gray-500">ID</dt><dd className="font-mono text-gray-700 dark:text-gray-300">{run.id}</dd>
          <dt className="text-gray-500">{t('runDetail.status')}</dt><dd>{run.status}</dd>
          <dt className="text-gray-500">{t('runDetail.duration')}</dt><dd>{(run.elapsed_ms / 1000).toFixed(2)}s</dd>
        </dl>
      </div>

      {run.inputs && Object.keys(run.inputs).length > 0 && (
        <div className="bg-white dark:bg-gray-900 rounded-xl border border-gray-200 dark:border-gray-800 p-4">
          <h3 className="text-sm font-semibold text-gray-700 dark:text-gray-300 mb-2">{t('runDetail.inputs')}</h3>
          <pre className="text-xs bg-gray-50 dark:bg-gray-800 p-3 rounded-lg overflow-x-auto">{JSON.stringify(run.inputs, null, 2)}</pre>
        </div>
      )}

      {run.outputs && Object.keys(run.outputs).length > 0 && (
        <div className="bg-white dark:bg-gray-900 rounded-xl border border-gray-200 dark:border-gray-800 p-4">
          <h3 className="text-sm font-semibold text-gray-700 dark:text-gray-300 mb-2">{t('runDetail.outputs')}</h3>
          <pre className="text-xs bg-gray-50 dark:bg-gray-800 p-3 rounded-lg overflow-x-auto">{JSON.stringify(run.outputs, null, 2)}</pre>
        </div>
      )}

      <div className="bg-white dark:bg-gray-900 rounded-xl border border-gray-200 dark:border-gray-800 p-4">
        <h3 className="text-sm font-semibold text-gray-700 dark:text-gray-300 mb-3">{t('runDetail.timeline', { count: events.length })}</h3>
        <EventTimeline events={events} />
      </div>
    </div>
  );
}
