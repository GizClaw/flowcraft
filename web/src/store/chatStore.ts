import { create } from 'zustand';
import type { ToolCallInfo, RichMessage } from '../types/chat';

export interface AgentChatSession {
  messages: RichMessage[];
  historyLoaded: boolean;
}

function emptySession(): AgentChatSession {
  return { messages: [], historyLoaded: false };
}

const EMPTY_SESSION: AgentChatSession = Object.freeze({ messages: [], historyLoaded: false }) as AgentChatSession;

interface StreamingState {
  isStreaming: boolean;
  content: string;
  toolCalls: ToolCallInfo[];
  abortController: AbortController | null;
}

function emptyStreaming(): StreamingState {
  return { isStreaming: false, content: '', toolCalls: [], abortController: null };
}

const EMPTY_STREAMING: StreamingState = Object.freeze({ isStreaming: false, content: '', toolCalls: [], abortController: null }) as StreamingState;

interface ChatState {
  sessions: Record<string, AgentChatSession>;
  streaming: Record<string, StreamingState>;

  ensureSession: (agentId: string) => void;
  getSession: (agentId: string) => AgentChatSession;
  getStreaming: (agentId: string) => StreamingState;
  loadHistory: (agentId: string, messages: RichMessage[]) => void;
  restoreFromHistory: (agentId: string, messages: RichMessage[]) => void;

  addUserMessage: (agentId: string, content: string) => void;

  startStreaming: (agentId: string) => AbortController;
  appendStreamChunk: (agentId: string, chunk: string) => void;
  addToolCall: (agentId: string, tc: ToolCallInfo) => void;
  updateToolCallResult: (agentId: string, id: string | undefined, name: string, result: string, status: 'success' | 'error') => void;
  commitIntermediateMessage: (agentId: string) => void;
  finishStreaming: (agentId: string, extra?: Partial<RichMessage>) => void;
  stopStreaming: (agentId: string) => void;

  isAgentStreaming: (agentId: string) => boolean;

  clearSession: (agentId: string) => void;
}

let msgCounter = 0;

export const useChatStore = create<ChatState>((set, get) => ({
  sessions: {},
  streaming: {},

  ensureSession: (agentId) => {
    if (!get().sessions[agentId]) {
      set({ sessions: { ...get().sessions, [agentId]: emptySession() } });
    }
  },

  getSession: (agentId) => get().sessions[agentId] || EMPTY_SESSION,

  getStreaming: (agentId) => get().streaming[agentId] || EMPTY_STREAMING,

  loadHistory: (agentId, messages) => {
    const session = get().sessions[agentId];
    if (session?.historyLoaded) return;

    const localMessages = session?.messages || [];
    let merged: RichMessage[];
    if (localMessages.length > 0 && messages.length > 0) {
      const historyIds = new Set(messages.map((m) => m.id));
      const localOnly = localMessages.filter((m) => !historyIds.has(m.id));
      merged = localOnly.length > 0 ? [...messages, ...localOnly] : messages;
    } else if (localMessages.length > 0) {
      merged = localMessages;
    } else {
      merged = messages;
    }

    set({
      sessions: {
        ...get().sessions,
        [agentId]: { ...(session || emptySession()), messages: merged, historyLoaded: true },
      },
    });
  },

  restoreFromHistory: (agentId, messages) => {
    const session = get().sessions[agentId] || emptySession();
    const st = get().streaming[agentId];
    if (st?.isStreaming) return;

    const historyIds = new Set(messages.map((m) => m.id));
    const localOnly = session.messages.filter((m) => !historyIds.has(m.id));
    const merged = localOnly.length > 0 ? [...messages, ...localOnly] : messages;

    set({
      sessions: {
        ...get().sessions,
        [agentId]: { ...session, messages: merged, historyLoaded: true },
      },
      streaming: {
        ...get().streaming,
        [agentId]: emptyStreaming(),
      },
    });
  },

  addUserMessage: (agentId, content) => {
    const session = get().sessions[agentId] || emptySession();
    const msg: RichMessage = {
      id: `chat-${++msgCounter}`,
      role: 'user',
      content,
      timestamp: new Date().toISOString(),
    };
    set({
      sessions: {
        ...get().sessions,
        [agentId]: { ...session, messages: [...session.messages, msg] },
      },
    });
  },

  startStreaming: (agentId) => {
    const controller = new AbortController();
    set({
      streaming: {
        ...get().streaming,
        [agentId]: { isStreaming: true, content: '', toolCalls: [], abortController: controller },
      },
    });
    return controller;
  },

  appendStreamChunk: (agentId, chunk) => {
    const st = get().streaming[agentId] || emptyStreaming();
    set({
      streaming: {
        ...get().streaming,
        [agentId]: { ...st, content: st.content + chunk },
      },
    });
  },

  addToolCall: (agentId, tc) => {
    const st = get().streaming[agentId] || emptyStreaming();
    set({
      streaming: {
        ...get().streaming,
        [agentId]: { ...st, toolCalls: [...st.toolCalls, tc] },
      },
    });
  },

  updateToolCallResult: (agentId, id, name, result, status) => {
    const st = get().streaming[agentId] || emptyStreaming();
    const tcs = [...st.toolCalls];
    const tc = id
      ? tcs.find((t) => t.id === id)
      : tcs.find((t) => t.name === name && t.status === 'pending');
    if (tc) {
      tc.result = result;
      tc.status = status;
      set({
        streaming: {
          ...get().streaming,
          [agentId]: { ...st, toolCalls: tcs },
        },
      });
      return;
    }
    const session = get().sessions[agentId];
    if (!session) return;
    const msgs = [...session.messages];
    for (let i = msgs.length - 1; i >= 0; i--) {
      const msg = msgs[i];
      if (!msg.toolCalls) continue;
      const committed = id
        ? msg.toolCalls.find((t) => t.id === id)
        : msg.toolCalls.find((t) => t.name === name && t.status === 'pending');
      if (committed) {
        msgs[i] = {
          ...msg,
          toolCalls: msg.toolCalls.map((t) =>
            t === committed ? { ...t, result, status } : t
          ),
        };
        set({ sessions: { ...get().sessions, [agentId]: { ...session, messages: msgs } } });
        return;
      }
    }
  },

  commitIntermediateMessage: (agentId) => {
    const st = get().streaming[agentId];
    if (!st) return;
    if (!st.content && st.toolCalls.length === 0) return;
    const session = get().sessions[agentId];
    if (!session) return;
    const msg: RichMessage = {
      id: `chat-${++msgCounter}`,
      role: 'assistant',
      content: st.content,
      toolCalls: st.toolCalls.length > 0 ? [...st.toolCalls] : undefined,
      timestamp: new Date().toISOString(),
    };
    set({
      sessions: {
        ...get().sessions,
        [agentId]: { ...session, messages: [...session.messages, msg] },
      },
      streaming: {
        ...get().streaming,
        [agentId]: { ...st, content: '', toolCalls: [] },
      },
    });
  },

  finishStreaming: (agentId, extra) => {
    const st = get().streaming[agentId] || emptyStreaming();
    const session = get().sessions[agentId];
    if (!session) {
      set({
        streaming: {
          ...get().streaming,
          [agentId]: emptyStreaming(),
        },
      });
      return;
    }
    const newMessages = [...session.messages];
    if (st.content || st.toolCalls.length > 0) {
      newMessages.push({
        id: `chat-${++msgCounter}`,
        role: 'assistant',
        content: st.content,
        toolCalls: st.toolCalls.length > 0 ? st.toolCalls : undefined,
        ...extra,
        timestamp: new Date().toISOString(),
      });
    }
    set({
      sessions: {
        ...get().sessions,
        [agentId]: { ...session, messages: newMessages },
      },
      streaming: {
        ...get().streaming,
        [agentId]: emptyStreaming(),
      },
    });
  },

  stopStreaming: (agentId) => {
    const st = get().streaming[agentId];
    if (st?.abortController) st.abortController.abort();
  },

  isAgentStreaming: (agentId) => {
    const st = get().streaming[agentId];
    return st?.isStreaming ?? false;
  },

  clearSession: (agentId) => {
    const sessions = { ...get().sessions };
    const streaming = { ...get().streaming };
    delete sessions[agentId];
    delete streaming[agentId];
    set({ sessions, streaming });
  },
}));
