import { useCallback, useMemo, useEffect } from 'react';
import {
  ReactFlow, Background, Controls, MiniMap,
  ReactFlowProvider, useReactFlow, MarkerType,
  type Node, type Edge, type NodeTypes, type Connection,
} from '@xyflow/react';
import CustomNode from './nodes/CustomNode';
import StartNode from './nodes/StartNode';
import EndNode from './nodes/EndNode';
import { useNodeTypeStore } from '../store/nodeTypeStore';
import { useWorkflowStore, type CustomNodeData } from '../store/workflowStore';
import { detectParallelSets } from '../utils/nodeHelpers';
import { minimapColorMap } from '../constants/colors';
import { deriveColorClasses } from '../types/nodeTypes';

const nodeTypes: NodeTypes = { custom: CustomNode, start: StartNode, end: EndNode };

function WorkflowCanvas() {
  const { screenToFlowPosition } = useReactFlow();
  const dynamicSchemas = useNodeTypeStore((s) => s.byType);

  const nodes = useWorkflowStore((s) => s.nodes);
  const edges = useWorkflowStore((s) => s.edges);
  const onNodesChange = useWorkflowStore((s) => s.onNodesChange);
  const onEdgesChange = useWorkflowStore((s) => s.onEdgesChange);
  const onConnect = useWorkflowStore((s) => s.onConnect);
  const addNode = useWorkflowStore((s) => s.addNode);
  const setSelectedNodeId = useWorkflowStore((s) => s.setSelectedNodeId);
  const setSelectedEdgeId = useWorkflowStore((s) => s.setSelectedEdgeId);
  const undo = useWorkflowStore((s) => s.undo);
  const redo = useWorkflowStore((s) => s.redo);

  const { forkEdgeIds, joinNodeIds } = useMemo(() => detectParallelSets(nodes, edges), [nodes, edges]);

  const styledEdges: Edge[] = useMemo(() =>
    edges.map((e) =>
      forkEdgeIds.has(e.id) ? { ...e, style: { ...e.style, strokeWidth: 2.5, stroke: '#3b82f6' }, animated: true } : e
    ), [edges, forkEdgeIds]);

  const styledNodes: Node[] = useMemo(() =>
    nodes.map((n) =>
      joinNodeIds.has(n.id) ? { ...n, className: `${n.className || ''} ring-2 ring-blue-400 ring-offset-1`.trim() } : n
    ), [nodes, joinNodeIds]);

  useEffect(() => {
    const handler = (e: KeyboardEvent) => {
      const tag = (e.target as HTMLElement)?.tagName;
      if (tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT') return;
      if ((e.metaKey || e.ctrlKey) && e.key === 'z' && !e.shiftKey) { e.preventDefault(); undo(); }
      if ((e.metaKey || e.ctrlKey) && ((e.key === 'z' && e.shiftKey) || e.key === 'y')) { e.preventDefault(); redo(); }
    };
    document.addEventListener('keydown', handler);
    return () => document.removeEventListener('keydown', handler);
  }, [undo, redo]);

  const isConnectionValid = useCallback((connection: Connection) => {
    if (!connection.source || !connection.target) return false;
    const sourceNode = nodes.find((n) => n.id === connection.source);
    const targetNode = nodes.find((n) => n.id === connection.target);
    if (!sourceNode || !targetNode) return false;
    const sourceType = (sourceNode.data as CustomNodeData).nodeType;
    const targetType = (targetNode.data as CustomNodeData).nodeType;
    if (targetType === 'start') return false;
    if (sourceType === 'end') return false;
    return true;
  }, [nodes]);

  const handleConnect = useCallback((connection: Connection) => {
    if (!isConnectionValid(connection)) return;
    onConnect(connection);
  }, [isConnectionValid, onConnect]);

  const onDragOver = useCallback((event: React.DragEvent) => {
    event.preventDefault();
    event.dataTransfer.dropEffect = 'move';
  }, []);

  const onDrop = useCallback((event: React.DragEvent) => {
    event.preventDefault();
    const nodeType = event.dataTransfer.getData('application/reactflow');
    if (!nodeType) return;
    const position = screenToFlowPosition({ x: event.clientX, y: event.clientY });
    const schema = dynamicSchemas[nodeType];
    const label = schema?.label || nodeType.charAt(0).toUpperCase() + nodeType.slice(1);
    const rfType = nodeType === 'end' ? 'end' : nodeType === 'start' ? 'start' : 'custom';

    const newNode: Node = {
      id: `${nodeType}_${crypto.randomUUID().slice(0, 8)}`,
      type: rfType,
      position,
      data: { label, nodeType, config: {} },
    };
    addNode(newNode);
    setSelectedNodeId(newNode.id);
  }, [screenToFlowPosition, addNode, setSelectedNodeId, dynamicSchemas]);

  const nodeColor = useCallback((node: Node) => {
    const nt = ((node.data as Record<string, unknown>)?.nodeType as string) || '';
    const s = dynamicSchemas[nt];
    if (s?.color) return minimapColorMap[deriveColorClasses(s.color).bg] || '#f3f4f6';
    return '#e0e7ff';
  }, [dynamicSchemas]);

  return (
    <ReactFlow
      nodes={styledNodes}
      edges={styledEdges}
      onNodesChange={onNodesChange}
      onEdgesChange={onEdgesChange}
      onConnect={handleConnect}
      onDragOver={onDragOver}
      onDrop={onDrop}
      onNodeClick={(_, node) => setSelectedNodeId(node.id)}
      onEdgeClick={(_, edge) => setSelectedEdgeId(edge.id)}
      onPaneClick={() => { setSelectedNodeId(null); setSelectedEdgeId(null); }}
      nodeTypes={nodeTypes}
      defaultEdgeOptions={{
        style: { strokeWidth: 2, stroke: '#94a3b8' },
        type: 'smoothstep',
        markerEnd: { type: MarkerType.ArrowClosed, width: 16, height: 16, color: '#94a3b8' },
      }}
      fitView
      className="bg-gray-50 dark:bg-gray-900"
      deleteKeyCode={['Backspace', 'Delete']}
    >
      <Background />
      <Controls />
      <MiniMap nodeColor={nodeColor} />
    </ReactFlow>
  );
}

export default function WorkflowEditor() {
  return (
    <ReactFlowProvider>
      <WorkflowCanvas />
    </ReactFlowProvider>
  );
}
