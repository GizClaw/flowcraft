import { describe, it, expect, beforeEach } from 'vitest';
import { useKanbanStore } from './kanbanStore';
import type { KanbanCard } from '../types/kanban';

function getState() {
  return useKanbanStore.getState();
}

function act(fn: () => void) {
  fn();
}

const NOW = '2024-01-01T00:00:00Z';

function makeCard(overrides: Partial<KanbanCard> = {}): KanbanCard {
  return {
    id: 'card-1',
    type: 'task',
    status: 'pending',
    producer: 'agent-main',
    consumer: '*',
    created_at: NOW,
    updated_at: NOW,
    ...overrides,
  };
}

beforeEach(() => {
  act(() => getState().reset());
});

describe('kanbanStore', () => {
  // ── applyEvent ──

  describe('applyEvent', () => {
    it('card_created adds a card', () => {
      const card = makeCard();
      act(() => getState().applyEvent({ type: 'card_created', card, timestamp: NOW }));

      expect(getState().cards.size).toBe(1);
      expect(getState().cards.get('card-1')).toEqual(card);
      expect(getState().events).toHaveLength(1);
    });

    it('card_claimed updates card status', () => {
      const card = makeCard();
      act(() => getState().applyEvent({ type: 'card_created', card, timestamp: NOW }));
      act(() => getState().applyEvent({
        type: 'card_claimed',
        card: { ...card, consumer: 'agent-1' },
        timestamp: NOW,
      }));

      const updated = getState().cards.get('card-1')!;
      expect(updated.status).toBe('claimed');
      expect(updated.consumer).toBe('agent-1');
    });

    it('card_done sets card to done', () => {
      const card = makeCard();
      act(() => getState().applyEvent({ type: 'card_created', card, timestamp: NOW }));
      act(() => getState().applyEvent({
        type: 'card_done',
        card: { ...card, output: 'result' },
        timestamp: NOW,
      }));

      expect(getState().cards.get('card-1')!.status).toBe('done');
    });

    it('card_failed sets card to failed', () => {
      const card = makeCard();
      act(() => getState().applyEvent({ type: 'card_created', card, timestamp: NOW }));
      act(() => getState().applyEvent({
        type: 'card_failed',
        card: { ...card, error: 'timeout' },
        timestamp: NOW,
      }));

      expect(getState().cards.get('card-1')!.status).toBe('failed');
    });

    it('handles card_claimed without prior card_created gracefully', () => {
      const card = makeCard();
      act(() => getState().applyEvent({
        type: 'card_claimed',
        card,
        timestamp: NOW,
      }));

      expect(getState().cards.size).toBe(1);
      expect(getState().cards.get('card-1')!.status).toBe('claimed');
    });

    it('handles card_done without prior card_created gracefully', () => {
      const card = makeCard({ id: 'orphan-done', output: 'result' });
      act(() => getState().applyEvent({
        type: 'card_done',
        card,
        timestamp: NOW,
      }));

      expect(getState().cards.size).toBe(1);
      expect(getState().cards.get('orphan-done')!.status).toBe('done');
      expect(getState().cards.get('orphan-done')!.output).toBe('result');
    });

    it('handles card_failed without prior card_created gracefully', () => {
      const card = makeCard({ id: 'orphan-fail', error: 'timeout' });
      act(() => getState().applyEvent({
        type: 'card_failed',
        card,
        timestamp: NOW,
      }));

      expect(getState().cards.size).toBe(1);
      expect(getState().cards.get('orphan-fail')!.status).toBe('failed');
      expect(getState().cards.get('orphan-fail')!.error).toBe('timeout');
    });
  });

  // ── Events truncation ──

  describe('events truncation', () => {
    it('truncates events when exceeding 200', () => {
      for (let i = 0; i < 201; i++) {
        act(() => getState().applyEvent({
          type: 'card_created',
          card: makeCard({ id: `card-${i}` }),
          timestamp: NOW,
        }));
      }

      expect(getState().events.length).toBeLessThanOrEqual(101);
    });
  });

  // ── loadSnapshot ──

  describe('loadSnapshot', () => {
    it('loads cards from API snapshot', () => {
      const cards: KanbanCard[] = [
        makeCard({ id: 'c1', status: 'pending' }),
        makeCard({ id: 'c2', status: 'done' }),
      ];

      act(() => getState().loadSnapshot(cards));

      expect(getState().cards.size).toBe(2);
      expect(getState().cards.get('c1')!.status).toBe('pending');
      expect(getState().cards.get('c2')!.status).toBe('done');
      expect(getState().events).toHaveLength(0);
    });

    it('replaces existing state', () => {
      act(() => getState().applyEvent({
        type: 'card_created',
        card: makeCard({ id: 'old' }),
        timestamp: NOW,
      }));
      expect(getState().cards.size).toBe(1);

      act(() => getState().loadSnapshot([makeCard({ id: 'new' })]));

      expect(getState().cards.size).toBe(1);
      expect(getState().cards.has('old')).toBe(false);
      expect(getState().cards.has('new')).toBe(true);
    });
  });

  // ── getCardsByStatus ──

  describe('getCardsByStatus', () => {
    it('filters cards by status', () => {
      act(() => getState().loadSnapshot([
        makeCard({ id: 'c1', status: 'pending' }),
        makeCard({ id: 'c2', status: 'done' }),
        makeCard({ id: 'c3', status: 'pending' }),
        makeCard({ id: 'c4', status: 'failed' }),
      ]));

      expect(getState().getCardsByStatus('pending')).toHaveLength(2);
      expect(getState().getCardsByStatus('done')).toHaveLength(1);
      expect(getState().getCardsByStatus('failed')).toHaveLength(1);
      expect(getState().getCardsByStatus('claimed')).toHaveLength(0);
    });
  });

  // ── runtimeId ──

  describe('setRuntimeId', () => {
    it('sets and clears runtime ID', () => {
      act(() => getState().setRuntimeId('runtime-42'));
      expect(getState().runtimeId).toBe('runtime-42');

      act(() => getState().setRuntimeId(null));
      expect(getState().runtimeId).toBeNull();
    });
  });

  // ── reset ──

  describe('reset', () => {
    it('clears all state', () => {
      act(() => getState().applyEvent({
        type: 'card_created',
        card: makeCard(),
        timestamp: NOW,
      }));
      act(() => getState().setRuntimeId('runtime-1'));
      act(() => getState().appendAgentToken('card-1', 'builder', 'hello'));

      act(() => getState().reset());

      expect(getState().cards.size).toBe(0);
      expect(getState().events).toHaveLength(0);
      expect(getState().runtimeId).toBeNull();
      expect(getState().agentDetails.size).toBe(0);
    });
  });

  // ── agentDetails ──

  describe('agentDetails', () => {
    it('appendAgentToken creates and accumulates', () => {
      act(() => getState().appendAgentToken('card-1', 'builder', 'hello '));
      act(() => getState().appendAgentToken('card-1', 'builder', 'world'));

      const detail = getState().agentDetails.get('card-1')!;
      expect(detail.content).toBe('hello world');
      expect(detail.graphId).toBe('builder');
      expect(detail.toolCalls).toHaveLength(0);
    });

    it('addAgentToolCall adds tool calls', () => {
      act(() => getState().addAgentToolCall('card-2', 'runner', {
        name: 'graph',
        args: '{"action":"get"}',
        status: 'pending',
      }));

      const detail = getState().agentDetails.get('card-2')!;
      expect(detail.toolCalls).toHaveLength(1);
      expect(detail.toolCalls[0].name).toBe('graph');
    });

    it('updateAgentToolResult updates the matching pending tool call', () => {
      act(() => getState().addAgentToolCall('card-3', 'builder', {
        name: 'graph',
        args: '{"action":"update","nodes":[]}',
        status: 'pending',
      }));

      act(() => getState().updateAgentToolResult('card-3', '', 'graph', 'ok', 'success'));

      const detail = getState().agentDetails.get('card-3')!;
      expect(detail.toolCalls[0].status).toBe('success');
      expect(detail.toolCalls[0].result).toBe('ok');
    });

    it('updateAgentToolResult does nothing for unknown card', () => {
      act(() => getState().updateAgentToolResult('nonexistent', '', 'tool', 'res', 'success'));
      expect(getState().agentDetails.size).toBe(0);
    });
  });

  // ── Full lifecycle ──

  describe('full lifecycle', () => {
    it('agent submits task → target agent claims → completes', () => {
      const card = makeCard({ id: 'task-1', query: 'fix bug' });

      act(() => getState().applyEvent({ type: 'card_created', card, timestamp: NOW }));

      expect(getState().getCardsByStatus('pending')).toHaveLength(1);

      act(() => getState().applyEvent({
        type: 'card_claimed',
        card: { ...card, consumer: 'coder-1' },
        timestamp: NOW,
      }));

      expect(getState().getCardsByStatus('claimed')).toHaveLength(1);

      act(() => getState().applyEvent({
        type: 'card_done',
        card: { ...card, output: 'bug fixed', consumer: 'coder-1' },
        timestamp: NOW,
      }));

      expect(getState().getCardsByStatus('done')).toHaveLength(1);
      expect(getState().cards.get('task-1')!.output).toBe('bug fixed');
      expect(getState().events).toHaveLength(3);
    });
  });

  // ── setAgentDetail ──

  describe('setAgentDetail', () => {
    it('sets detail for a card', () => {
      act(() => getState().setAgentDetail('card-set-1', {
        cardId: 'card-set-1',
        graphId: 'g1',
        content: 'restored content',
        toolCalls: [{ id: 'tc1', name: 'search', args: '{}', status: 'success', result: 'ok' }],
      }));

      const detail = getState().agentDetails.get('card-set-1');
      expect(detail).toBeDefined();
      expect(detail!.content).toBe('restored content');
      expect(detail!.toolCalls).toHaveLength(1);
      expect(detail!.toolCalls[0].name).toBe('search');
    });

    it('overwrites existing detail', () => {
      act(() => getState().setAgentDetail('card-overwrite', {
        cardId: 'card-overwrite', graphId: '', content: 'old', toolCalls: [],
      }));
      act(() => getState().setAgentDetail('card-overwrite', {
        cardId: 'card-overwrite', graphId: '', content: 'new', toolCalls: [],
      }));

      expect(getState().agentDetails.get('card-overwrite')!.content).toBe('new');
    });
  });

  // ── run_id propagation ──

  describe('run_id propagation', () => {
    it('card_created with run_id stores it', () => {
      const card = makeCard({ id: 'run-card', run_id: 'run-abc' });
      act(() => getState().applyEvent({ type: 'card_created', card, timestamp: NOW }));

      const stored = getState().cards.get('run-card');
      expect(stored).toBeDefined();
      expect(stored!.run_id).toBe('run-abc');
    });

    it('card_done updates run_id', () => {
      const card = makeCard({ id: 'run-done' });
      act(() => getState().applyEvent({ type: 'card_created', card, timestamp: NOW }));
      act(() => getState().applyEvent({
        type: 'card_done',
        card: { ...card, run_id: 'run-xyz', output: 'done' },
        timestamp: NOW,
      }));

      expect(getState().cards.get('run-done')!.run_id).toBe('run-xyz');
    });
  });
});
