import { useState } from 'react';
import { useTranslation } from 'react-i18next';
import { ReactFlowProvider } from '@xyflow/react';
import KanbanBoard from '../components/kanban/KanbanBoard';
import KanbanTimeline from '../components/kanban/KanbanTimeline';
import KanbanTopology from '../components/kanban/KanbanTopology';
import RuntimeDebugPanel from '../components/runtime/RuntimeDebugPanel';
import { useKanbanBoard } from '../hooks/useKanbanBoard';
import EmptyState from '../components/common/EmptyState';
import { OWNER_RUNTIME_ID } from '../utils/runtime';

export default function GlobalKanbanPage() {
  const { t } = useTranslation();
  const [runtimeId] = useState<string>(OWNER_RUNTIME_ID);
  const [view, setView] = useState<'board' | 'timeline' | 'topology'>('board');

  const viewLabels: Record<string, string> = {
    board: t('kanbanPage.board'),
    timeline: t('kanbanPage.timeline'),
    topology: t('kanbanPage.topology'),
  };

  useKanbanBoard(runtimeId);

  return (
    <div className="flex flex-col h-full">
      <div className="flex items-center gap-4 px-4 py-3 border-b border-gray-200 dark:border-gray-800 bg-white dark:bg-gray-900 shrink-0">
        <span className="text-sm font-medium text-gray-700 dark:text-gray-300">{t('kanbanPage.title')}</span>
        <div className="flex gap-1">
          {(['board', 'timeline', 'topology'] as const).map((v) => (
            <button
              key={v}
              onClick={() => setView(v)}
              className={`px-3 py-1.5 text-sm rounded-lg ${view === v ? 'bg-indigo-100 dark:bg-indigo-900 text-indigo-700 dark:text-indigo-300 font-medium' : 'text-gray-500 hover:bg-gray-100 dark:hover:bg-gray-800'}`}
            >
              {viewLabels[v]}
            </button>
          ))}
        </div>
      </div>
      <RuntimeDebugPanel runtimeId={runtimeId} />
      <div className="flex-1 overflow-auto">
        {!runtimeId ? (
          <EmptyState title={t('kanbanPage.notStarted')} description={t('kanbanPage.notStartedDesc')} />
        ) : view === 'board' ? (
          <KanbanBoard />
        ) : view === 'timeline' ? (
          <KanbanTimeline runtimeId={runtimeId} />
        ) : (
          <ReactFlowProvider>
            <KanbanTopology runtimeId={runtimeId} />
          </ReactFlowProvider>
        )}
      </div>
    </div>
  );
}
