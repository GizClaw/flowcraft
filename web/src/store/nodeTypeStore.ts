import { create } from 'zustand';
import { nodeTypeApi } from '../utils/api';
import type { NodeSchema } from '../types/nodeTypes';

interface NodeTypeState {
  schemas: NodeSchema[];
  byType: Record<string, NodeSchema>;
  loading: boolean;
  error: string | null;
  fetched: boolean;

  fetchNodeTypes: () => Promise<void>;
  getSchema: (nodeType: string) => NodeSchema | undefined;
  getByCategory: (category: string) => NodeSchema[];
  getVisibleByCategory: (category: string) => NodeSchema[];
  getCategories: () => string[];
}

export const useNodeTypeStore = create<NodeTypeState>((set, get) => ({
  schemas: [],
  byType: {},
  loading: false,
  error: null,
  fetched: false,

  fetchNodeTypes: async () => {
    if (get().loading) return;
    set({ loading: true, error: null });
    try {
      const schemas = await nodeTypeApi.list();
      const byType: Record<string, NodeSchema> = {};
      for (const s of schemas) byType[s.type] = s;
      set({ schemas, byType, loading: false, fetched: true });
    } catch (err) {
      set({ loading: false, error: err instanceof Error ? err.message : 'Failed to fetch', fetched: true });
    }
  },

  getSchema: (nodeType) => get().byType[nodeType],
  getByCategory: (category) => get().schemas.filter((s) => s.category === category),
  getVisibleByCategory: (category) => get().schemas.filter((s) => s.category === category && !s.deprecated),
  getCategories: () => [...new Set(get().schemas.map((s) => s.category))],
}));
