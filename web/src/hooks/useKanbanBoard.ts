import { useEffect, useRef } from 'react';
import { useKanbanStore } from '../store/kanbanStore';
import { useCoPilotStore } from '../store/copilotStore';
import { useChatStore } from '../store/chatStore';
import { kanbanApi } from '../utils/api';
import { useWebSocket } from './useWebSocket';
import type { KanbanEvent } from '../types/kanban';
import { mapCardStatusToUI } from '../types/kanban';
import type { Envelope } from '../eventlog/types';
import { envelopeRouter } from '../eventlog/router';
import { registerChatReducersOnce } from '../eventlog/chatReducers';

let kanbanReducersRegistered = false;
function registerKanbanReducersOnce() {
  if (kanbanReducersRegistered) return;
  kanbanReducersRegistered = true;
  envelopeRouter.on('task.submitted', (e) => {
    useKanbanStore.getState().applyTaskSubmitted(e as Envelope<{ card_id: string; runtime_id: string; target_agent_id?: string; query?: string }>);
    useCoPilotStore.getState().updateDispatchedTaskStatus((e.payload as { card_id: string }).card_id, mapCardStatusToUI('pending'));
  });
  envelopeRouter.on('task.claimed', (e) => {
    useKanbanStore.getState().applyTaskClaimed(e as Envelope<{ card_id: string; runtime_id: string; target_agent_id?: string }>);
    useCoPilotStore.getState().updateDispatchedTaskStatus((e.payload as { card_id: string }).card_id, mapCardStatusToUI('claimed'));
  });
  envelopeRouter.on('task.completed', (e) => {
    useKanbanStore.getState().applyTaskCompleted(e as Envelope<{ card_id: string; runtime_id: string; target_agent_id?: string; result?: string; elapsed_ms?: number }>);
    useCoPilotStore.getState().updateDispatchedTaskStatus((e.payload as { card_id: string }).card_id, mapCardStatusToUI('done'));
    maybeStopBackground();
  });
  envelopeRouter.on('task.failed', (e) => {
    useKanbanStore.getState().applyTaskFailed(e as Envelope<{ card_id: string; runtime_id: string; target_agent_id?: string; error?: string; elapsed_ms?: number }>);
    useCoPilotStore.getState().updateDispatchedTaskStatus((e.payload as { card_id: string }).card_id, mapCardStatusToUI('failed'));
    maybeStopBackground();
  });
}

function maybeStopBackground() {
  const store = useKanbanStore.getState();
  const hasPending = store.getCardsByStatus('pending').length > 0 || store.getCardsByStatus('claimed').length > 0;
  if (!hasPending) {
    useCoPilotStore.getState().setBackgroundRunning(false);
  }
}

// isEnvelope returns true when the WS frame matches the §4 envelope wire
// format (seq+partition+type+payload). The legacy `{type:"kanban", ...}`
// frames returned false so they continue to flow through the legacy
// switch below.
function isEnvelope(msg: Record<string, unknown>): boolean {
  return (
    typeof msg.seq === 'number' &&
    typeof msg.partition === 'string' &&
    typeof msg.type === 'string' &&
    'payload' in msg
  );
}

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
// NOTE: the previous `pendingCallbackMessages` global map was removed in
// R4. All chat-relevant frames are now envelopes routed through
// `envelopeRouter`, which preserves order via `seq`. The legacy
// callback_* WS frames below remain for backward-compat with any
// non-event-sourced code paths still emitting them; they will be
// removed once R5 retires the legacy WS transport.

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
  return processCallbackMessage(msg, chatAgentId);
}

export function resetCallbackMessageStateForTest() {
  seenKeys.splice(0, seenKeys.length);
  seenSet.clear();
  activeCallbackCards.clear();
}

function handleKanbanMessage(data: unknown, callbackAgentId?: string) {
  const msg = data as Record<string, unknown>;
  if (!msg.type) return;

  // Envelope frames go through the central router (§7.1.5). Each
  // registered reducer (KanbanStore, CoPilotStore, ...) updates its own
  // slice of state from the payload.
  if (isEnvelope(msg)) {
    envelopeRouter.dispatch(msg as unknown as Envelope);
    return;
  }

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

// fullResync grabs the current /kanban/cards snapshot once at boot or on
// WS reconnect. Per §7.1.5 the steady-state KanbanStore is fed by
// envelopes only — fullResync is the only legacy fetch that survives R3
// (it covers events from before the WS connected).
//
// After R5 the response also carries `last_seq`; we hand it to the store
// so the next envelope frame is reconciled against the snapshot cursor
// (drops anything ≤ last_seq, which would otherwise double-apply).
function fullResync() {
  kanbanApi.cards()
    .then((snap) => useKanbanStore.getState().loadSnapshot(snap.cards, snap.lastSeq))
    .catch((err) => console.error('kanban: snapshot load failed:', err));
}

export function useKanbanBoard(runtimeId: string | null, callbackAgentId?: string) {
  const setRuntimeId = useKanbanStore((s) => s.setRuntimeId);
  const reset = useKanbanStore((s) => s.reset);
  const prevRuntimeRef = useRef<string | null>(null);
  const callbackAgentIdRef = useRef(callbackAgentId);
  callbackAgentIdRef.current = callbackAgentId;

  useEffect(() => {
    registerKanbanReducersOnce();
    registerChatReducersOnce();
  }, []);

  useEffect(() => {
    if (!runtimeId) {
      if (prevRuntimeRef.current !== null) reset();
      prevRuntimeRef.current = null;
      return;
    }
    setRuntimeId(runtimeId);
    fullResync();
    prevRuntimeRef.current = runtimeId;
  }, [runtimeId, setRuntimeId, reset]);

  const wsUrl = runtimeId ? '/api/ws' : null;

  useWebSocket(wsUrl, {
    onMessage: (data) => handleKanbanMessage(data, callbackAgentIdRef.current),
    onOpen: () => {
      if (runtimeId) fullResync();
    },
    reconnectInterval: 5000,
  });
}
