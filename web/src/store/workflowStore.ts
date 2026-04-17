import { create } from 'zustand';
import {
  addEdge,
  applyNodeChanges,
  applyEdgeChanges,
  type Node,
  type Edge,
  type NodeChange,
  type EdgeChange,
  type Connection,
} from '@xyflow/react';
import type { NodeStatus } from '../utils/streaming';

export interface CustomNodeData {
  label: string;
  nodeType: string;
  config: Record<string, unknown>;
  [key: string]: unknown;
}

interface GraphSnapshot {
  nodes: Node[];
  edges: Edge[];
}

interface WorkflowState {
  nodes: Node[];
  edges: Edge[];
  selectedNodeId: string | null;
  selectedEdgeId: string | null;

  past: GraphSnapshot[];
  future: GraphSnapshot[];

  nodeStatuses: Map<string, NodeStatus>;
  nodeWarnings: Map<string, string[]>;
  nodeErrors: Map<string, string[]>;
  draftChecksum: string;
  isDirty: boolean;

  onNodesChange: (changes: NodeChange[]) => void;
  onEdgesChange: (changes: EdgeChange[]) => void;
  onConnect: (connection: Connection) => void;

  addNode: (node: Node) => void;
  deleteNode: (nodeId: string) => void;
  setSelectedNodeId: (nodeId: string | null) => void;
  setSelectedEdgeId: (edgeId: string | null) => void;
  updateNodeConfig: (nodeId: string, config: Partial<Record<string, unknown>>) => void;
  updateNodeLabel: (nodeId: string, label: string) => void;
  updateEdgeData: (edgeId: string, data: Record<string, unknown>) => void;

  pushHistory: () => void;
  undo: () => void;
  redo: () => void;
  canUndo: () => boolean;
  canRedo: () => boolean;

  setNodeStatus: (nodeId: string, status: NodeStatus) => void;
  setNodeStatuses: (statuses: Map<string, NodeStatus>) => void;
  clearNodeStatuses: () => void;
  setNodeWarnings: (warnings: Map<string, string[]>) => void;
  setNodeErrors: (errors: Map<string, string[]>) => void;
  setDirty: (dirty: boolean) => void;

  loadGraph: (nodes: Node[], edges: Edge[]) => void;
  reset: () => void;
}

const MAX_HISTORY = 50;

const initialNodes: Node[] = [
  { id: '__start__', type: 'start', data: { label: 'Start', nodeType: 'start', config: {} }, position: { x: 250, y: 50 } },
  { id: '__end__', type: 'end', data: { label: 'End', nodeType: 'end', config: {} }, position: { x: 250, y: 400 } },
];
const initialEdges: Edge[] = [];

export const useWorkflowStore = create<WorkflowState>((set, get) => ({
  nodes: initialNodes,
  edges: initialEdges,
  selectedNodeId: null,
  selectedEdgeId: null,
  past: [],
  future: [],
  nodeStatuses: new Map(),
  nodeWarnings: new Map(),
  nodeErrors: new Map(),
  draftChecksum: '',
  isDirty: false,

  pushHistory: () => {
    const { nodes, edges, past } = get();
    const snapshot: GraphSnapshot = { nodes: structuredClone(nodes), edges: structuredClone(edges) };
    const newPast = [...past, snapshot].slice(-MAX_HISTORY);
    set({ past: newPast, future: [], isDirty: true });
  },

  undo: () => {
    const { past, nodes, edges } = get();
    if (past.length === 0) return;
    const prev = past[past.length - 1];
    set({
      past: past.slice(0, -1),
      future: [{ nodes: structuredClone(nodes), edges: structuredClone(edges) }, ...get().future],
      nodes: structuredClone(prev.nodes),
      edges: structuredClone(prev.edges),
      selectedNodeId: null,
      selectedEdgeId: null,
    });
  },

  redo: () => {
    const { future, nodes, edges } = get();
    if (future.length === 0) return;
    const next = future[0];
    set({
      future: future.slice(1),
      past: [...get().past, { nodes: structuredClone(nodes), edges: structuredClone(edges) }],
      nodes: structuredClone(next.nodes),
      edges: structuredClone(next.edges),
      selectedNodeId: null,
      selectedEdgeId: null,
    });
  },

  canUndo: () => get().past.length > 0,
  canRedo: () => get().future.length > 0,

  onNodesChange: (changes) => {
    const { selectedNodeId } = get();
    const isSelectedRemoved = selectedNodeId && changes.some((c) => c.type === 'remove' && c.id === selectedNodeId);
    const hasPositionChange = changes.some((c) => c.type === 'position' && c.position);
    set({
      nodes: applyNodeChanges(changes, get().nodes),
      ...(isSelectedRemoved ? { selectedNodeId: null } : {}),
      ...(hasPositionChange ? { isDirty: true } : {}),
    });
  },

  onEdgesChange: (changes) => {
    const { selectedEdgeId } = get();
    const edgeRemoved = selectedEdgeId && changes.some((c) => c.type === 'remove' && c.id === selectedEdgeId);
    set({
      edges: applyEdgeChanges(changes, get().edges),
      ...(edgeRemoved ? { selectedEdgeId: null } : {}),
    });
  },

  onConnect: (connection) => {
    get().pushHistory();
    set({ edges: addEdge({ ...connection, type: 'smoothstep' }, get().edges) });
  },

  addNode: (node) => {
    get().pushHistory();
    set({ nodes: [...get().nodes, node] });
  },

  deleteNode: (nodeId) => {
    get().pushHistory();
    set({
      nodes: get().nodes.filter((n) => n.id !== nodeId),
      edges: get().edges.filter((e) => e.source !== nodeId && e.target !== nodeId),
      selectedNodeId: get().selectedNodeId === nodeId ? null : get().selectedNodeId,
    });
  },

  setSelectedNodeId: (nodeId) => set({ selectedNodeId: nodeId, selectedEdgeId: null }),
  setSelectedEdgeId: (edgeId) => set({ selectedEdgeId: edgeId, selectedNodeId: null }),

  updateNodeConfig: (nodeId, config) => {
    set({
      nodes: get().nodes.map((node) => {
        if (node.id !== nodeId) return node;
        const data = node.data as CustomNodeData;
        return { ...node, data: { ...data, config: { ...data.config, ...config } } };
      }),
      isDirty: true,
    });
  },

  updateNodeLabel: (nodeId, label) => {
    set({
      nodes: get().nodes.map((n) => (n.id !== nodeId ? n : { ...n, data: { ...n.data, label } })),
    });
  },

  updateEdgeData: (edgeId, data) => {
    set({
      edges: get().edges.map((e) => {
        if (e.id !== edgeId) return e;
        const condition = data.condition as string | undefined;
        return { ...e, data: { ...e.data, ...data }, label: condition || undefined };
      }),
      isDirty: true,
    });
  },

  setNodeStatus: (nodeId, status) => {
    const next = new Map(get().nodeStatuses);
    next.set(nodeId, status);
    set({ nodeStatuses: next });
  },

  setNodeStatuses: (statuses) => set({ nodeStatuses: statuses }),
  clearNodeStatuses: () => set({ nodeStatuses: new Map() }),

  setNodeWarnings: (warnings) => set({ nodeWarnings: warnings }),
  setNodeErrors: (errors) => set({ nodeErrors: errors }),
  setDirty: (dirty) => set({ isDirty: dirty }),

  loadGraph: (nodes, edges) => {
    set({
      nodes,
      edges,
      selectedNodeId: null,
      selectedEdgeId: null,
      past: [],
      future: [],
      nodeStatuses: new Map(),
      nodeWarnings: new Map(),
      nodeErrors: new Map(),
      isDirty: false,
    });
  },

  reset: () => {
    set({
      nodes: structuredClone(initialNodes),
      edges: structuredClone(initialEdges),
      selectedNodeId: null,
      selectedEdgeId: null,
      past: [],
      future: [],
      nodeStatuses: new Map(),
      nodeWarnings: new Map(),
      nodeErrors: new Map(),
      isDirty: false,
      draftChecksum: '',
    });
  },
}));
