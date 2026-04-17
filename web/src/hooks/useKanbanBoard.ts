import { useEffect, useRef } from 'react';
import { useKanbanStore } from '../store/kanbanStore';
import { useCoPilotStore } from '../store/copilotStore';
import { useChatStore } from '../store/chatStore';
import { kanbanApi } from '../utils/api';
import { useWebSocket } from './useWebSocket';
import type { KanbanEvent } from '../types/kanban';
import { mapCardStatusToUI } from '../types/kanban';

interface AgentStreamPayload {
  card_id?: string;
  graph_id?: string;
  chunk?: string;
  tool_name?: string;
  tool_call_id?: string;
  tool_args?: string;
  tool_result?: string;
  is_error?: boolean;
  timestamp?: string;
}

interface CallbackStartPayload {
  card_id?: string;
  runtime_id?: string;
  agent_id?: string;
  query?: string;
}

interface CallbackDonePayload {
  card_id?: string;
  runtime_id?: string;
  agent_id?: string;
  error?: string;
}

const DEDUP_SIZE = 256;
const seenKeys: string[] = [];
const seenSet = new Set<string>();
const activeCallbackCards = new Set<string>();
const pendingCallbackMessages = new Map<string, Array<Record<string, unknown>>>();
const pendingCallbackTimers = new Map<string, ReturnType<typeof setTimeout>>();

function isDuplicate(key: string): boolean {
  if (seenSet.has(key)) return true;
  if (seenKeys.length >= DEDUP_SIZE) {
    const evicted = seenKeys.shift()!;
    seenSet.delete(evicted);
  }
  seenKeys.push(key);
  seenSet.add(key);
  return false;
}

function callbackCardID(msg: Record<string, unknown>): string {
  return typeof msg.card_id === 'string' ? msg.card_id : '';
}

function isChatAgentBusy(chatAgentId: string): boolean {
  return useChatStore.getState().isAgentStreaming(chatAgentId);
}

function shouldQueueCallbackMessage(msg: Record<string, unknown>, chatAgentId: string): boolean {
  const type = typeof msg.type === 'string' ? msg.type : '';
  if (!type.startsWith('callback_')) return false;
  const cardID = callbackCardID(msg);
  if (cardID && activeCallbackCards.has(cardID)) {
    return false;
  }
  return isChatAgentBusy(chatAgentId);
}

function queueCallbackMessage(msg: Record<string, unknown>, chatAgentId: string) {
  const queue = pendingCallbackMessages.get(chatAgentId) || [];
  queue.push(msg);
  pendingCallbackMessages.set(chatAgentId, queue);
  schedulePendingCallbackDrain(chatAgentId);
}

function schedulePendingCallbackDrain(chatAgentId: string) {
  if (pendingCallbackTimers.has(chatAgentId)) return;
  const tick = () => {
    pendingCallbackTimers.delete(chatAgentId);
    if (isChatAgentBusy(chatAgentId)) {
      pendingCallbackTimers.set(chatAgentId, setTimeout(tick, 25));
      return;
    }
    const queue = pendingCallbackMessages.get(chatAgentId);
    if (!queue || queue.length === 0) {
      pendingCallbackMessages.delete(chatAgentId);
      return;
    }
    while (queue.length > 0) {
      processCallbackMessage(queue.shift()!, chatAgentId);
    }
    pendingCallbackMessages.delete(chatAgentId);
  };
  pendingCallbackTimers.set(chatAgentId, setTimeout(tick, 25));
}

function processCallbackMessage(
  msg: Record<string, unknown>,
  chatAgentId: string,
): boolean {
  const store = useChatStore.getState();

/**
 * Handle callback streaming messages by delegating to chatStore.
 * Supports callback_start, callback_token, callback_tool_call,
 * callback_tool_result, and callback_done.
 */
  switch (msg.type) {
    case 'callback_start': {
      const p = msg as unknown as CallbackStartPayload;
      const dedupKey = `cb_start:${p?.card_id || ''}`;
      if (isDuplicate(dedupKey)) return true;
      if (p?.card_id) activeCallbackCards.add(p.card_id);
      if (p?.query) {
        store.addUserMessage(chatAgentId, p.query);
      }
      store.startStreaming(chatAgentId);
      return true;
    }
    case 'callback_token': {
      const p = msg as unknown as AgentStreamPayload;
      if (p?.chunk) {
        const dedupKey = `cb_token:${p.card_id || ''}:${p.timestamp || ''}:${p.chunk.slice(0, 32)}`;
        if (isDuplicate(dedupKey)) return true;
        store.appendStreamChunk(chatAgentId, p.chunk);
      }
      return true;
    }
    case 'callback_tool_call': {
      const p = msg as unknown as AgentStreamPayload;
      if (p?.tool_name) {
        const dedupKey = `cb_tool_call:${p.card_id || ''}:${p.tool_call_id || p.tool_name}`;
        if (isDuplicate(dedupKey)) return true;
        const st = store.getStreaming(chatAgentId);
        if (st.content) {
          store.commitIntermediateMessage(chatAgentId);
        }
        store.addToolCall(chatAgentId, {
          id: p.tool_call_id,
          name: p.tool_name,
          args: p.tool_args || '',
          status: 'pending',
        });
      }
      return true;
    }
    case 'callback_tool_result': {
      const p = msg as unknown as AgentStreamPayload;
      if (p?.tool_name) {
        const dedupKey = `cb_tool_result:${p.card_id || ''}:${p.tool_call_id || p.tool_name}:${p.timestamp || ''}`;
        if (isDuplicate(dedupKey)) return true;
        store.updateToolCallResult(
          chatAgentId,
          p.tool_call_id,
          p.tool_name,
          p.tool_result || '',
          p.is_error ? 'error' : 'success',
        );
      }
      return true;
    }
    case 'callback_done': {
      const p = msg as unknown as CallbackDonePayload;
      const dedupKey = `cb_done:${p?.card_id || ''}`;
      if (isDuplicate(dedupKey)) return true;
      if (p?.error) {
        const st = store.getStreaming(chatAgentId);
        const prefix = st.content ? '\n\n' : '';
        store.appendStreamChunk(chatAgentId, `${prefix}Error: ${p.error}`);
      }
      store.finishStreaming(chatAgentId, { isCallback: true, cardId: p?.card_id });
      if (p?.card_id) activeCallbackCards.delete(p.card_id);
      return true;
    }
    default:
      return false;
  }
}

export function handleCallbackMessage(
  msg: Record<string, unknown>,
  chatAgentId: string,
): boolean {
  if (shouldQueueCallbackMessage(msg, chatAgentId)) {
    queueCallbackMessage(msg, chatAgentId);
    return true;
  }
  return processCallbackMessage(msg, chatAgentId);
}

export function resetCallbackMessageStateForTest() {
  seenKeys.splice(0, seenKeys.length);
  seenSet.clear();
  activeCallbackCards.clear();
  pendingCallbackMessages.clear();
  for (const timer of pendingCallbackTimers.values()) {
    clearTimeout(timer);
  }
  pendingCallbackTimers.clear();
}

function handleKanbanMessage(data: unknown, callbackAgentId?: string) {
  const msg = data as Record<string, unknown>;
  if (!msg.type) return;

  // Delegate callback_* messages to the callback handler.
  // Only consume if the event's agent_id matches the panel's callbackAgentId.
  if (callbackAgentId && (msg.type as string).startsWith('callback_')) {
    const eventAgentId = typeof msg.agent_id === 'string' ? msg.agent_id : '';
    if (!eventAgentId || eventAgentId === callbackAgentId) {
      handleCallbackMessage(msg, callbackAgentId);
    }
    return;
  }

  switch (msg.type) {
    case 'kanban': {
      const ev = msg.payload as unknown as KanbanEvent;
      if (!ev) return;
      const dedupKey = `kanban:${ev.type}:${ev.card?.id || ''}:${ev.card?.status || ''}`;
      if (isDuplicate(dedupKey)) return;
      useKanbanStore.getState().applyEvent(ev);
      if (ev.card?.id && ev.card.status) {
        const uiStatus = mapCardStatusToUI(ev.card.status);
        useCoPilotStore.getState().updateDispatchedTaskStatus(ev.card.id, uiStatus);
      }
      if (ev.type === 'card_done' || ev.type === 'card_failed') {
        const store = useKanbanStore.getState();
        const hasPending = store.getCardsByStatus('pending').length > 0 || store.getCardsByStatus('claimed').length > 0;
        if (!hasPending) {
          useCoPilotStore.getState().setBackgroundRunning(false);
        }
      }
      break;
    }
    case 'agent_token': {
      const p = msg as unknown as AgentStreamPayload;
      if (p.card_id && p.chunk) {
        const dedupKey = `token:${p.card_id}:${p.timestamp || ''}:${p.chunk?.slice(0, 32)}`;
        if (isDuplicate(dedupKey)) return;
        useKanbanStore.getState().appendAgentToken(p.card_id, p.graph_id || '', p.chunk);
      }
      break;
    }
    case 'agent_tool_call': {
      const p = msg as unknown as AgentStreamPayload;
      if (p.card_id && p.tool_name) {
        const dedupKey = `tool_call:${p.card_id}:${p.tool_call_id || p.tool_name}`;
        if (isDuplicate(dedupKey)) return;
        useKanbanStore.getState().addAgentToolCall(p.card_id, p.graph_id || '', {
          id: p.tool_call_id,
          name: p.tool_name,
          args: p.tool_args || '',
          status: 'pending',
        });
      }
      break;
    }
    case 'agent_tool_result': {
      const p = msg as unknown as AgentStreamPayload;
      if (p.card_id && p.tool_name) {
        const dedupKey = `tool_result:${p.card_id}:${p.tool_call_id || p.tool_name}:${p.timestamp || ''}`;
        if (isDuplicate(dedupKey)) return;
        useKanbanStore.getState().updateAgentToolResult(
          p.card_id,
          p.tool_call_id || '',
          p.tool_name,
          p.tool_result || '',
          p.is_error ? 'error' : 'success',
        );
      }
      break;
    }
  }
}

export function useKanbanBoard(runtimeId: string | null, callbackAgentId?: string) {
  const loadSnapshot = useKanbanStore((s) => s.loadSnapshot);
  const setRuntimeId = useKanbanStore((s) => s.setRuntimeId);
  const reset = useKanbanStore((s) => s.reset);
  const prevRuntimeRef = useRef<string | null>(null);
  const snapshotLoadedRef = useRef(false);
  const callbackAgentIdRef = useRef(callbackAgentId);
  callbackAgentIdRef.current = callbackAgentId;

  useEffect(() => {
    if (!runtimeId) {
      if (prevRuntimeRef.current !== null) reset();
      prevRuntimeRef.current = null;
      snapshotLoadedRef.current = false;
      return;
    }

    setRuntimeId(runtimeId);

    let timer: ReturnType<typeof setInterval> | undefined;
    let cancelled = false;

    const tryLoad = () => {
      kanbanApi.cards()
        .then((cards) => {
          if (cancelled) return;
          snapshotLoadedRef.current = true;
          loadSnapshot(cards);
          if (timer) { clearInterval(timer); timer = undefined; }
        })
        .catch((err) => console.error('kanban: snapshot load failed:', err));
    };

    tryLoad();
    timer = setInterval(tryLoad, 3000);
    prevRuntimeRef.current = runtimeId;

    return () => {
      cancelled = true;
      if (timer) clearInterval(timer);
    };
  }, [runtimeId, loadSnapshot, setRuntimeId, reset]);

  const wsUrl = runtimeId ? '/api/ws' : null;

  useWebSocket(wsUrl, {
    onMessage: (data) => handleKanbanMessage(data, callbackAgentIdRef.current),
    onOpen: () => {
      if (runtimeId) {
        kanbanApi.cards()
          .then((cards) => useKanbanStore.getState().loadSnapshot(cards))
          .catch((err) => console.error('kanban: reconnect snapshot load failed:', err));
      }
    },
    reconnectInterval: 5000,
  });
}
