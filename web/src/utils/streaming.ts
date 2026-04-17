import type { WorkflowStreamEvent } from '../types/chat';

export type NodeStatus = 'idle' | 'running' | 'completed' | 'skipped' | 'error';

export interface StreamState {
  nodeStatuses: Map<string, NodeStatus>;
  events: WorkflowStreamEvent[];
  answer: string;
  isRunning: boolean;
  error?: string;
  runId?: string;
}

export function createStreamState(): StreamState {
  return {
    nodeStatuses: new Map(),
    events: [],
    answer: '',
    isRunning: false,
  };
}

export function applyStreamEvent(state: StreamState, event: WorkflowStreamEvent): StreamState {
  const next = { ...state, events: [...state.events, event] };

  switch (event.type) {
    case 'graph_start':
      next.isRunning = true;
      next.runId = event.run_id;
      next.nodeStatuses = new Map();
      next.answer = '';
      next.error = undefined;
      break;

    case 'node_start':
      if (event.node_id) {
        next.nodeStatuses = new Map(state.nodeStatuses);
        next.nodeStatuses.set(event.node_id, 'running');
      }
      break;

    case 'node_complete':
      if (event.node_id) {
        next.nodeStatuses = new Map(state.nodeStatuses);
        next.nodeStatuses.set(event.node_id, 'completed');
      }
      break;

    case 'node_skipped':
      if (event.node_id) {
        next.nodeStatuses = new Map(state.nodeStatuses);
        next.nodeStatuses.set(event.node_id, 'skipped');
      }
      break;

    case 'node_error':
      if (event.node_id) {
        next.nodeStatuses = new Map(state.nodeStatuses);
        next.nodeStatuses.set(event.node_id, 'error');
      }
      break;

    case 'agent_token':
      if (event.chunk) {
        next.answer = state.answer + event.chunk;
      }
      break;

    case 'error':
      next.error = event.error;
      next.isRunning = false;
      break;

    case 'graph_end':
    case 'done':
      next.isRunning = false;
      if (event.output) {
        next.answer = event.output.answer || next.answer;
      }
      break;
  }

  return next;
}
