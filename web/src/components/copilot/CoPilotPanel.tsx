import { useEffect, useCallback, useState, useMemo, useRef } from 'react';
import { useNavigate } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { X, Bot, Sparkles, Settings, Loader, MessageSquare, ListTodo, Bell, ArrowRight } from 'lucide-react';
import { useCoPilotStore, COPILOT_AGENT_ID } from '../../store/copilotStore';
import { useChatStore } from '../../store/chatStore';
import { useKanbanStore } from '../../store/kanbanStore';
import { useUIStore } from '../../store/uiStore';
import { useWorkflowStore, type CustomNodeData } from '../../store/workflowStore';
import { useKanbanBoard } from '../../hooks/useKanbanBoard';
import RichChatView from '../chat/RichChatView';
import CoPilotTasks from './CoPilotTasks';
import TaskDispatchCard from './TaskDispatchCard';
import CoPilotInput from './CoPilotInput';
import { useCoPilot } from '../../hooks/useCoPilot';
import { chatApi, kanbanApi } from '../../utils/api';
import { OWNER_RUNTIME_ID, getRuntimeConversationId } from '../../utils/runtime';
import { mapHistoryMessages } from './utils/mapHistoryMessages';
import type { KanbanCard } from '../../types/kanban';
import type { RichMessage } from '../../types/chat';

function hasRunningTasks(cards: KanbanCard[]): boolean {
  return cards.some((c) => c.status === 'claimed' || c.status === 'pending');
}

type TabType = 'chat' | 'tasks';

function CallbackBanner({ cardId, onViewTasks }: { cardId?: string; onViewTasks?: () => void }) {
  const { t } = useTranslation();
  return (
    <div className="flex items-center gap-2 px-3 py-1.5 bg-emerald-50 dark:bg-emerald-950/30 border border-emerald-200 dark:border-emerald-800 rounded-lg text-emerald-700 dark:text-emerald-300 text-xs">
      <Bell size={12} />
      <span>{t('copilot.callbackBanner')}{cardId && <span className="font-mono ml-1 opacity-70">{cardId.slice(0, 8)}</span>}</span>
      {cardId && onViewTasks && (
        <button onClick={onViewTasks} className="ml-auto flex items-center gap-0.5 text-emerald-600 hover:text-emerald-800 dark:text-emerald-400 dark:hover:text-emerald-200">
          {t('copilot.viewDetails')} <ArrowRight size={10} />
        </button>
      )}
    </div>
  );
}

export default function CoPilotPanel() {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const open = useUIStore((s) => s.copilotOpen);
  const setCopilotOpen = useUIStore((s) => s.setCopilotOpen);

  const backgroundRunning = useCoPilotStore((s) => s.backgroundRunning);
  const setBackgroundRunning = useCoPilotStore((s) => s.setBackgroundRunning);
  const setGraphContext = useCoPilotStore((s) => s.setGraphContext);

  const copilotConversationId = getRuntimeConversationId(COPILOT_AGENT_ID);
  const isMyStreaming = useChatStore((s) => s.isAgentStreaming(copilotConversationId));
  const ensureSession = useChatStore((s) => s.ensureSession);
  const loadHistory = useChatStore((s) => s.loadHistory);
  const restoreFromHistory = useChatStore((s) => s.restoreFromHistory);
  const session = useChatStore((s) => s.getSession(copilotConversationId));

  const kanbanCards = useKanbanStore((s) => s.cards);
  const nodes = useWorkflowStore((s) => s.nodes);
  const edges = useWorkflowStore((s) => s.edges);

  useKanbanBoard(OWNER_RUNTIME_ID, COPILOT_AGENT_ID);
  const recoveryAttemptedRef = useRef(false);
  const [activeTab, setActiveTab] = useState<TabType>('chat');

  const { sendMessage, stopStreaming } = useCoPilot();

  useEffect(() => { ensureSession(copilotConversationId); }, [copilotConversationId, ensureSession]);

  useEffect(() => {
    const nodeTypes: Record<string, number> = {};
    let userNodeCount = 0;
    nodes.forEach((n) => {
      if (n.id !== '__start__' && n.id !== '__end__') {
        userNodeCount++;
        const data = n.data as CustomNodeData;
        const type = data.nodeType || 'unknown';
        nodeTypes[type] = (nodeTypes[type] || 0) + 1;
      }
    });
    const summary = userNodeCount === 0
      ? 'Empty graph'
      : `${userNodeCount} node${userNodeCount !== 1 ? 's' : ''}, ${edges.length} edge${edges.length !== 1 ? 's' : ''}`;
    setGraphContext({ nodeCount: userNodeCount, edgeCount: edges.length, nodeTypes, summary });
  }, [nodes, edges, setGraphContext]);

  const runningTaskCount = useMemo(
    () => Array.from(kanbanCards.values()).filter(
      (c) => c.status === 'claimed' || c.status === 'pending'
    ).length,
    [kanbanCards]
  );

  const loadHistoryAndCheck = useCallback(async () => {
    const historyLoaded = session.historyLoaded;
    try {
      const [messages, snap] = await Promise.all([
        chatApi.getMessages(copilotConversationId),
        kanbanApi.cards().catch(() => ({ cards: [] as KanbanCard[], lastSeq: 0, lastEventTs: null, realmId: null })),
      ]);
      const cards = snap.cards;

      if (messages.length > 0) {
        const mapped = mapHistoryMessages(messages);
        if (historyLoaded) {
          restoreFromHistory(copilotConversationId, mapped);
        } else {
          loadHistory(copilotConversationId, mapped);
        }
      } else {
        if (!historyLoaded) loadHistory(copilotConversationId, []);
      }

      if (hasRunningTasks(cards)) {
        setBackgroundRunning(true);
      } else {
        setBackgroundRunning(false);
      }
    } catch {
      if (!session.historyLoaded) loadHistory(copilotConversationId, []);
    }
  }, [copilotConversationId, session.historyLoaded, loadHistory, restoreFromHistory, setBackgroundRunning]);

  useEffect(() => {
    if (!open) {
      recoveryAttemptedRef.current = false;
      return;
    }
    if (recoveryAttemptedRef.current) return;
    recoveryAttemptedRef.current = true;
    if (isMyStreaming) return;
    loadHistoryAndCheck();
  }, [open, loadHistoryAndCheck, isMyStreaming]);

  const switchToTasks = useCallback(() => setActiveTab('tasks'), []);

  const renderExtra = useCallback((msg: RichMessage) => {
    return (
      <>
        {msg.isCallback && <CallbackBanner cardId={msg.cardId} onViewTasks={switchToTasks} />}
        {msg.dispatchedTask && <TaskDispatchCard task={msg.dispatchedTask} onViewTasks={switchToTasks} />}
      </>
    );
  }, [switchToTasks]);

  if (!open) {
    return (
      <button
        onClick={() => setCopilotOpen(true)}
        className="fixed bottom-6 right-6 w-14 h-14 rounded-full bg-indigo-600 text-white shadow-lg hover:bg-indigo-700 flex items-center justify-center z-50 transition-transform hover:scale-105"
        title="Open CoPilot"
      >
        <Sparkles size={24} />
      </button>
    );
  }

  return (
    <div className="fixed bottom-6 right-6 w-[560px] h-[780px] max-h-[calc(100vh-3rem)] bg-white dark:bg-gray-900 rounded-2xl shadow-2xl border border-gray-200 dark:border-gray-800 flex flex-col z-50 overflow-hidden">
      {/* Header */}
      <div className="flex items-center justify-between px-4 py-3 bg-indigo-600 text-white shrink-0">
        <div className="flex items-center gap-2">
          <Bot size={18} />
          <h3 className="text-sm font-semibold">{t('copilot.title')}</h3>
        </div>
        <div className="flex items-center gap-1">
          <button
            onClick={() => { navigate('/global-settings#copilot'); setCopilotOpen(false); }}
            className="p-1.5 rounded-lg hover:bg-indigo-700 text-white/80 hover:text-white transition-colors"
            title={t('copilot.settings')}
          >
            <Settings size={16} />
          </button>
          <button
            onClick={() => setCopilotOpen(false)}
            className="p-1.5 rounded-lg hover:bg-indigo-700 text-white/80 hover:text-white transition-colors"
            title={t('common.close')}
          >
            <X size={16} />
          </button>
        </div>
      </div>

      {/* Tabs */}
      <div className="flex border-b border-gray-200 dark:border-gray-700 shrink-0">
        <button
          onClick={() => setActiveTab('chat')}
          className={`flex-1 flex items-center justify-center gap-1.5 px-3 py-2 text-xs font-medium transition-colors ${
            activeTab === 'chat'
              ? 'text-indigo-600 dark:text-indigo-400 border-b-2 border-indigo-600 dark:border-indigo-400'
              : 'text-gray-500 dark:text-gray-400 hover:text-gray-700 dark:hover:text-gray-300'
          }`}
        >
          <MessageSquare size={14} />
          {t('copilot.chat')}
        </button>
        <button
          onClick={() => setActiveTab('tasks')}
          className={`flex-1 flex items-center justify-center gap-1.5 px-3 py-2 text-xs font-medium transition-colors ${
            activeTab === 'tasks'
              ? 'text-indigo-600 dark:text-indigo-400 border-b-2 border-indigo-600 dark:border-indigo-400'
              : 'text-gray-500 dark:text-gray-400 hover:text-gray-700 dark:hover:text-gray-300'
          }`}
        >
          <ListTodo size={14} />
          {t('copilot.tasks')}
          {runningTaskCount > 0 && (
            <span className="inline-flex items-center justify-center w-4 h-4 rounded-full bg-violet-500 text-white text-[10px] font-bold">
              {runningTaskCount}
            </span>
          )}
        </button>
      </div>

      {/* Background running banner */}
      {backgroundRunning && !isMyStreaming && (
        <div className="flex items-center gap-2 px-4 py-2 bg-amber-50 dark:bg-amber-900/30 border-b border-amber-200 dark:border-amber-800 text-amber-700 dark:text-amber-300 text-xs">
          <Loader size={12} className="animate-spin" />
          <span>{t('copilot.backgroundRunning')}</span>
        </div>
      )}

      {/* Content */}
      {activeTab === 'chat' ? (
        <RichChatView
          agentId={COPILOT_AGENT_ID}
          renderExtra={renderExtra}
          emptyIcon={<Sparkles size={32} className="text-indigo-300 mb-3" />}
          emptyTitle="I can help you build and manage workflows."
          emptyHint='Try: "Create a chatbot agent" or "Add an LLM node"'
          inputComponent={
            <CoPilotInput onSend={sendMessage} onStop={stopStreaming} isStreaming={isMyStreaming} />
          }
        />
      ) : (
        <div className="flex-1 overflow-y-auto p-4">
          <CoPilotTasks />
        </div>
      )}
    </div>
  );
}
