import { describe, it, expect, beforeEach } from 'vitest';
import { useCoPilotStore, labelKeyForTemplate, labelForTemplate, COPILOT_AGENT_ID } from './copilotStore';

function s() { return useCoPilotStore.getState(); }

beforeEach(() => {
  useCoPilotStore.setState({
    currentAgentId: null,
    backgroundRunning: false,
    dispatchedTasks: new Map(),
    graphContext: { nodeCount: 0, edgeCount: 0, nodeTypes: {}, summary: '' },
  });
});

describe('copilotStore', () => {
  // ── Constants ──

  describe('COPILOT_AGENT_ID', () => {
    it('equals copilot', () => {
      expect(COPILOT_AGENT_ID).toBe('copilot');
    });
  });

  // ── backgroundRunning ──

  describe('backgroundRunning', () => {
    it('defaults to false', () => {
      expect(s().backgroundRunning).toBe(false);
    });

    it('toggles correctly', () => {
      s().setBackgroundRunning(true);
      expect(s().backgroundRunning).toBe(true);
      s().setBackgroundRunning(false);
      expect(s().backgroundRunning).toBe(false);
    });
  });

  // ── currentAgentId ──

  describe('currentAgentId', () => {
    it('defaults to null', () => {
      expect(s().currentAgentId).toBeNull();
    });

    it('sets and clears agent id', () => {
      s().setCurrentAgentId('agent-7');
      expect(s().currentAgentId).toBe('agent-7');
      s().setCurrentAgentId(null);
      expect(s().currentAgentId).toBeNull();
    });
  });

  // ── Dispatched tasks ──

  describe('trackDispatchedTask', () => {
    it('adds a dispatched task', () => {
      s().trackDispatchedTask({ cardId: 'card-1', template: 'copilot_builder', status: 'submitted' });
      expect(s().dispatchedTasks.size).toBe(1);
      expect(s().dispatchedTasks.get('card-1')!.template).toBe('copilot_builder');
    });

    it('tracks multiple tasks independently', () => {
      s().trackDispatchedTask({ cardId: 'c1', template: 'copilot_builder', status: 'submitted' });
      s().trackDispatchedTask({ cardId: 'c2', template: 'copilot_builder', status: 'submitted' });
      expect(s().dispatchedTasks.size).toBe(2);
    });

    it('overwrites existing task with same cardId', () => {
      s().trackDispatchedTask({ cardId: 'c1', template: 'builder', status: 'submitted' });
      s().trackDispatchedTask({ cardId: 'c1', template: 'runner', status: 'running' });
      expect(s().dispatchedTasks.size).toBe(1);
      expect(s().dispatchedTasks.get('c1')!.template).toBe('runner');
    });
  });

  describe('updateDispatchedTaskStatus', () => {
    it('updates task status through lifecycle', () => {
      s().trackDispatchedTask({ cardId: 'card-2', template: 'copilot_builder', status: 'submitted' });

      s().updateDispatchedTaskStatus('card-2', 'running');
      expect(s().dispatchedTasks.get('card-2')!.status).toBe('running');

      s().updateDispatchedTaskStatus('card-2', 'success');
      expect(s().dispatchedTasks.get('card-2')!.status).toBe('success');
    });

    it('does nothing for unknown card', () => {
      s().updateDispatchedTaskStatus('nonexistent', 'error');
      expect(s().dispatchedTasks.size).toBe(0);
    });

    it('handles error status', () => {
      s().trackDispatchedTask({ cardId: 'c1', template: 't', status: 'running' });
      s().updateDispatchedTaskStatus('c1', 'error');
      expect(s().dispatchedTasks.get('c1')!.status).toBe('error');
    });
  });

  // ── GraphContext ──

  describe('graphContext', () => {
    it('defaults to empty', () => {
      expect(s().graphContext.nodeCount).toBe(0);
      expect(s().graphContext.edgeCount).toBe(0);
      expect(s().graphContext.summary).toBe('');
    });

    it('sets graph context', () => {
      s().setGraphContext({ nodeCount: 3, edgeCount: 2, nodeTypes: { llm: 2, script: 1 }, summary: '3 nodes' });
      expect(s().graphContext.nodeCount).toBe(3);
      expect(s().graphContext.edgeCount).toBe(2);
      expect(s().graphContext.nodeTypes.llm).toBe(2);
      expect(s().graphContext.nodeTypes.script).toBe(1);
      expect(s().graphContext.summary).toBe('3 nodes');
    });

    it('overwrites previous context', () => {
      s().setGraphContext({ nodeCount: 1, edgeCount: 0, nodeTypes: {}, summary: 'a' });
      s().setGraphContext({ nodeCount: 5, edgeCount: 4, nodeTypes: { llm: 5 }, summary: 'b' });
      expect(s().graphContext.nodeCount).toBe(5);
      expect(s().graphContext.summary).toBe('b');
    });
  });
});

// ── Pure functions ──

describe('labelKeyForTemplate', () => {
  it('returns i18n key for known templates', () => {
    expect(labelKeyForTemplate('copilot_builder')).toBe('copilot.agent.builder');
  });

  it('returns null for unknown templates', () => {
    expect(labelKeyForTemplate('custom_agent')).toBeNull();
    expect(labelKeyForTemplate('')).toBeNull();
  });
});

describe('labelForTemplate', () => {
  it('returns i18n key for known templates', () => {
    expect(labelForTemplate('copilot_builder')).toBe('copilot.agent.builder');
  });

  it('returns template name as-is for unknown', () => {
    expect(labelForTemplate('my_custom')).toBe('my_custom');
  });
});
