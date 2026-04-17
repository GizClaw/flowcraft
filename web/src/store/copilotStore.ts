import { create } from 'zustand';
import type { DispatchedTask } from '../types/chat';

export interface GraphContext {
  nodeCount: number;
  edgeCount: number;
  nodeTypes: Record<string, number>;
  summary: string;
}

const subAgentI18nKeys: Record<string, string> = {
  copilot_builder: 'copilot.agent.builder',
};

export function labelKeyForTemplate(template: string): string | null {
  return subAgentI18nKeys[template] || null;
}

export function labelForTemplate(template: string): string {
  return subAgentI18nKeys[template] || template;
}

export const COPILOT_AGENT_ID = 'copilot';

interface CoPilotState {
  currentAgentId: string | null;
  backgroundRunning: boolean;
  dispatchedTasks: Map<string, DispatchedTask>;
  graphContext: GraphContext;

  setCurrentAgentId: (id: string | null) => void;
  setBackgroundRunning: (running: boolean) => void;
  trackDispatchedTask: (task: DispatchedTask) => void;
  updateDispatchedTaskStatus: (cardId: string, status: DispatchedTask['status']) => void;
  setGraphContext: (context: GraphContext) => void;
}

export const useCoPilotStore = create<CoPilotState>((set, get) => ({
  currentAgentId: null,
  backgroundRunning: false,
  dispatchedTasks: new Map(),
  graphContext: { nodeCount: 0, edgeCount: 0, nodeTypes: {}, summary: '' },

  setCurrentAgentId: (id) => set({ currentAgentId: id }),
  setBackgroundRunning: (running) => set({ backgroundRunning: running }),

  trackDispatchedTask: (task) => {
    const tasks = new Map(get().dispatchedTasks);
    tasks.set(task.cardId, task);
    set({ dispatchedTasks: tasks });
  },

  updateDispatchedTaskStatus: (cardId, status) => {
    const tasks = new Map(get().dispatchedTasks);
    const task = tasks.get(cardId);
    if (!task) return;
    tasks.set(cardId, { ...task, status });
    set({ dispatchedTasks: tasks });
  },

  setGraphContext: (context) => set({ graphContext: context }),
}));
