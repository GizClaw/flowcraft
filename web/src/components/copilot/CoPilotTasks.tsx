import { useState, useEffect, useRef } from 'react';
import { useTranslation } from 'react-i18next';
import { CheckCircle, XCircle, Loader2, ChevronRight, ChevronDown, Clock, Cpu } from 'lucide-react';
import { useKanbanStore } from '../../store/kanbanStore';
import type { AgentDetail } from '../../store/kanbanStore';
import type { KanbanCard } from '../../types/kanban';
import type { ToolCallInfo } from '../../types/chat';
import { labelKeyForTemplate } from '../../store/copilotStore';
import { workflowRunApi } from '../../utils/api';
import ToolCallList from '../message/ToolCallList';
import MarkdownContent from '../message/MarkdownContent';

function TaskCard({ card, detail }: { card: KanbanCard; detail?: AgentDetail }) {
  const { t } = useTranslation();
  const [expanded, setExpanded] = useState(card.status === 'claimed');

  const isRunning = card.status === 'claimed';
  const isDone = card.status === 'done';
  const isFailed = card.status === 'failed';

  const borderColor = isRunning
    ? 'border-violet-300 dark:border-violet-700'
    : isDone
      ? 'border-emerald-200 dark:border-emerald-800'
      : isFailed
        ? 'border-red-300 dark:border-red-700'
        : 'border-gray-200 dark:border-gray-700';

  const bgColor = isRunning
    ? 'bg-violet-50/50 dark:bg-violet-950/30'
    : isDone
      ? 'bg-emerald-50/30 dark:bg-emerald-950/20'
      : isFailed
        ? 'bg-red-50/30 dark:bg-red-950/20'
        : 'bg-gray-50/50 dark:bg-gray-800/50';

  const statusIcon = isRunning
    ? <Loader2 size={14} className="animate-spin text-violet-500" />
    : isDone
      ? <CheckCircle size={14} className="text-emerald-500" />
      : isFailed
        ? <XCircle size={14} className="text-red-500" />
        : <Clock size={14} className="text-gray-400" />;

  const displayName = card.target_agent_id || card.template || card.consumer || 'unknown';
  const i18nKey = card.template ? labelKeyForTemplate(card.template) : null;
  const templateLabel = i18nKey ? t(i18nKey) : displayName;

  return (
    <div className={`border rounded-lg overflow-hidden ${borderColor} ${bgColor}`}>
      <button
        onClick={() => setExpanded(!expanded)}
        className="w-full flex items-center gap-2 px-3 py-2.5 text-left min-w-0"
      >
        {expanded
          ? <ChevronDown size={14} className="text-gray-400 shrink-0" />
          : <ChevronRight size={14} className="text-gray-400 shrink-0" />}
        <Cpu size={13} className="text-violet-500 shrink-0" />
        <span className="text-xs font-semibold text-gray-700 dark:text-gray-300 shrink-0">{templateLabel}</span>
        <span className="text-[11px] text-gray-500 dark:text-gray-400">{t(`copilot.taskCard.${card.status}`)}</span>
        {card.elapsed_ms != null && card.elapsed_ms > 0 && (
          <span className="text-[10px] text-gray-400">{(card.elapsed_ms / 1000).toFixed(1)}s</span>
        )}
        <span className="shrink-0 ml-auto">{statusIcon}</span>
      </button>

      {expanded && (
        <div className="border-t border-gray-200 dark:border-gray-700 px-3 py-2.5 space-y-2">
          {card.query && (
            <div>
              <p className="text-[10px] font-semibold text-gray-400 uppercase mb-1">{t('copilot.taskCard.instruction')}</p>
              <p className="text-xs text-gray-600 dark:text-gray-300 whitespace-pre-wrap line-clamp-4">{card.query}</p>
            </div>
          )}

          {detail && detail.toolCalls.length > 0 && (
            <div>
              <p className="text-[10px] font-semibold text-gray-400 uppercase mb-1">{t('copilot.taskCard.toolCalls')}</p>
              <ToolCallList toolCalls={detail.toolCalls} />
            </div>
          )}

          {detail && detail.content && (
            <div>
              <p className="text-[10px] font-semibold text-gray-400 uppercase mb-1">{t('copilot.taskCard.output')}</p>
              <div className="text-sm max-h-60 overflow-y-auto">
                <MarkdownContent content={detail.content} />
              </div>
            </div>
          )}

          {!detail && card.output && (
            <div>
              <p className="text-[10px] font-semibold text-gray-400 uppercase mb-1">{t('copilot.taskCard.result')}</p>
              <p className="text-xs text-gray-600 dark:text-gray-300 whitespace-pre-wrap line-clamp-6">{card.output}</p>
            </div>
          )}

          {card.error && (
            <div>
              <p className="text-[10px] font-semibold text-red-400 uppercase mb-1">{t('copilot.taskCard.error')}</p>
              <p className="text-xs text-red-600 dark:text-red-400 whitespace-pre-wrap">{card.error}</p>
            </div>
          )}

          {isRunning && !detail && (
            <div className="flex items-center gap-1.5 px-1 py-1">
              <div className="w-1.5 h-1.5 rounded-full bg-violet-400 animate-bounce [animation-delay:0ms]" />
              <div className="w-1.5 h-1.5 rounded-full bg-violet-400 animate-bounce [animation-delay:150ms]" />
              <div className="w-1.5 h-1.5 rounded-full bg-violet-400 animate-bounce [animation-delay:300ms]" />
            </div>
          )}
        </div>
      )}
    </div>
  );
}

function useRestoreAgentDetails(cards: Map<string, KanbanCard>) {
  const restored = useRef(new Set<string>());
  const agentDetails = useKanbanStore((s) => s.agentDetails);
  const setAgentDetail = useKanbanStore((s) => s.setAgentDetail);

  useEffect(() => {
    for (const card of cards.values()) {
      if (agentDetails.has(card.id)) continue;
      if (restored.current.has(card.id)) continue;
      if (card.status !== 'done' && card.status !== 'failed') continue;
      if (!card.run_id) continue;

      restored.current.add(card.id);
      workflowRunApi.get(card.run_id).then((run) => {
        if (!run) return;
        const toolCalls = (run.outputs?.tool_calls as ToolCallInfo[] | undefined) ?? [];
        setAgentDetail(card.id, {
          cardId: card.id,
          graphId: '',
          content: run.output ?? '',
          toolCalls,
        });
      }).catch(() => {});
    }
  }, [cards, agentDetails, setAgentDetail]);
}

export default function CoPilotTasks() {
  const cards = useKanbanStore((s) => s.cards);
  const agentDetails = useKanbanStore((s) => s.agentDetails);
  useRestoreAgentDetails(cards);

  const allCards = Array.from(cards.values()).sort((a, b) => {
    const order: Record<string, number> = { claimed: 0, pending: 1, failed: 2, done: 3 };
    const diff = (order[a.status] ?? 4) - (order[b.status] ?? 4);
    if (diff !== 0) return diff;
    return new Date(b.created_at).getTime() - new Date(a.created_at).getTime();
  });

  const { t } = useTranslation();

  if (allCards.length === 0) {
    return (
      <div className="flex flex-col items-center justify-center h-full text-center">
        <Cpu size={32} className="text-gray-300 dark:text-gray-600 mb-3" />
        <p className="text-sm text-gray-500 dark:text-gray-400">{t('copilot.emptyTasks')}</p>
        <p className="text-xs text-gray-400 mt-1">{t('copilot.emptyTasksHint')}</p>
      </div>
    );
  }

  return (
    <div className="space-y-2">
      {allCards.map((card) => (
        <TaskCard key={card.id} card={card} detail={agentDetails.get(card.id)} />
      ))}
    </div>
  );
}
