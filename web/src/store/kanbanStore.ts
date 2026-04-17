import { create } from 'zustand';
import type { KanbanCard, KanbanEvent, CardStatus } from '../types/kanban';
import type { ToolCallInfo } from '../types/chat';

export interface AgentDetail {
  cardId: string;
  graphId: string;
  content: string;
  toolCalls: ToolCallInfo[];
}

interface KanbanState {
  cards: Map<string, KanbanCard>;
  events: KanbanEvent[];
  runtimeId: string | null;
  agentDetails: Map<string, AgentDetail>;

  // Cached cards grouped by status for O(1) lookup
  cardsByStatus: Map<CardStatus, KanbanCard[]>;

  applyEvent: (event: KanbanEvent) => void;
  loadSnapshot: (cards: KanbanCard[]) => void;
  setRuntimeId: (id: string | null) => void;
  reset: () => void;

  // Selector: returns cached cards filtered by status (O(1) instead of O(n))
  getCardsByStatus: (status: CardStatus) => KanbanCard[];

  appendAgentToken: (cardId: string, graphId: string, chunk: string) => void;
  addAgentToolCall: (cardId: string, graphId: string, tc: ToolCallInfo) => void;
  updateAgentToolResult: (cardId: string, toolCallId: string, toolName: string, result: string, status: 'success' | 'error') => void;
  setAgentDetail: (cardId: string, detail: AgentDetail) => void;
}

// Helper to build cardsByStatus cache from cards map
function buildCardsByStatus(cards: Map<string, KanbanCard>): Map<CardStatus, KanbanCard[]> {
  const cache = new Map<CardStatus, KanbanCard[]>();
  cache.set('pending', []);
  cache.set('claimed', []);
  cache.set('done', []);
  cache.set('failed', []);
  cards.forEach((card) => {
    const list = cache.get(card.status);
    if (list) list.push(card);
  });
  return cache;
}

export const useKanbanStore = create<KanbanState>((set, get) => ({
  cards: new Map(),
  events: [],
  runtimeId: null,
  agentDetails: new Map(),
  cardsByStatus: new Map(),

  applyEvent: (event) => {
    const { cards, events } = get();
    const nextCards = new Map(cards);

    switch (event.type) {
      case 'card_created':
        if (event.card) nextCards.set(event.card.id, event.card);
        break;
      case 'card_claimed':
        if (event.card) nextCards.set(event.card.id, { ...nextCards.get(event.card.id)!, ...event.card, status: 'claimed' });
        break;
      case 'card_done':
        if (event.card) nextCards.set(event.card.id, { ...nextCards.get(event.card.id)!, ...event.card, status: 'done' });
        break;
      case 'card_failed':
        if (event.card) nextCards.set(event.card.id, { ...nextCards.get(event.card.id)!, ...event.card, status: 'failed' });
        break;
    }

    const nextEvents = [...events, event];
    set({
      cards: nextCards,
      events: nextEvents.length > 200 ? nextEvents.slice(-100) : nextEvents,
      cardsByStatus: buildCardsByStatus(nextCards),
    });
  },

  loadSnapshot: (cards) => {
    const cardMap = new Map<string, KanbanCard>();
    cards.forEach((c) => cardMap.set(c.id, c));
    set({ cards: cardMap, events: [], cardsByStatus: buildCardsByStatus(cardMap) });
  },

  setRuntimeId: (id) => set({ runtimeId: id }),

  reset: () => set({ cards: new Map(), events: [], runtimeId: null, agentDetails: new Map(), cardsByStatus: new Map() }),

  getCardsByStatus: (status) => {
    // O(1) lookup from cached map instead of O(n) iteration
    return get().cardsByStatus.get(status) || [];
  },

  setAgentDetail: (cardId, detail) => {
    const details = new Map(get().agentDetails);
    details.set(cardId, detail);
    set({ agentDetails: details });
  },

  appendAgentToken: (cardId, graphId, chunk) => {
    const details = new Map(get().agentDetails);
    const existing = details.get(cardId) || { cardId, graphId, content: '', toolCalls: [] };
    details.set(cardId, { ...existing, content: existing.content + chunk });
    set({ agentDetails: details });
  },

  addAgentToolCall: (cardId, graphId, tc) => {
    const details = new Map(get().agentDetails);
    const existing = details.get(cardId) || { cardId, graphId, content: '', toolCalls: [] };
    if (tc.id && existing.toolCalls.some((t) => t.id === tc.id)) return;
    if (!tc.id && existing.toolCalls.some((t) => t.name === tc.name && t.status === 'pending')) return;
    details.set(cardId, { ...existing, toolCalls: [...existing.toolCalls, tc] });
    set({ agentDetails: details });
  },

  updateAgentToolResult: (cardId, toolCallId, toolName, result, status) => {
    const details = new Map(get().agentDetails);
    const existing = details.get(cardId);
    if (!existing) return;
    const tcs = [...existing.toolCalls];
    const tc = (toolCallId && tcs.find((t) => t.id === toolCallId))
      || tcs.find((t) => t.name === toolName && t.status === 'pending');
    if (tc) {
      tc.result = result;
      tc.status = status;
      details.set(cardId, { ...existing, toolCalls: tcs });
      set({ agentDetails: details });
    }
  },
}));
