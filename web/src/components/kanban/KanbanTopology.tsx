import { useEffect, useState } from 'react';
import {
  ReactFlow,
  Background,
  Controls,
  MarkerType,
  type Node,
  type Edge,
} from '@xyflow/react';
import Dagre from '@dagrejs/dagre';
import { kanbanApi } from '../../utils/api';
import type { TopologyNode, TopologyEdge } from '../../types/kanban';

interface Props {
  runtimeId: string;
}

const NODE_WIDTH = 140;
const NODE_HEIGHT = 40;

function layoutGraph(topoNodes: TopologyNode[], topoEdges: TopologyEdge[]) {
  const g = new Dagre.graphlib.Graph().setDefaultEdgeLabel(() => ({}));
  g.setGraph({ rankdir: 'LR', ranksep: 80, nodesep: 50 });

  topoNodes.forEach((n) => g.setNode(n.id, { width: NODE_WIDTH, height: NODE_HEIGHT }));
  topoEdges.forEach((e, i) => g.setEdge(e.source, e.target, { id: `te-${i}` }));

  Dagre.layout(g);

  const nodes: Node[] = topoNodes.map((n) => {
    const pos = g.node(n.id);
    return {
      id: n.id,
      data: { label: n.name },
      position: { x: pos.x - NODE_WIDTH / 2, y: pos.y - NODE_HEIGHT / 2 },
      style: {
        background: '#eef2ff',
        color: '#312e81',
        border: '2px solid #818cf8',
        borderRadius: '8px',
        padding: '6px 16px',
        fontSize: '12px',
        fontWeight: 500,
        width: NODE_WIDTH,
      },
    };
  });

  const edges: Edge[] = topoEdges.map((e, i) => ({
    id: `te-${i}`,
    source: e.source,
    target: e.target,
    label: e.type,
    type: 'smoothstep',
    style: { strokeWidth: 2, stroke: '#94a3b8' },
    markerEnd: { type: MarkerType.ArrowClosed, width: 16, height: 16, color: '#94a3b8' },
    labelStyle: { fontSize: 10, fill: '#64748b' },
  }));

  return { nodes, edges };
}

export default function KanbanTopology({ runtimeId }: Props) {
  const [nodes, setNodes] = useState<Node[]>([]);
  const [edges, setEdges] = useState<Edge[]>([]);

  useEffect(() => {
    kanbanApi.topology().then(({ nodes: tn, edges: te }) => {
      const layout = layoutGraph(tn, te);
      setNodes(layout.nodes);
      setEdges(layout.edges);
    }).catch(() => {});
  }, [runtimeId]);

  return (
    <div className="h-full">
      <ReactFlow nodes={nodes} edges={edges} fitView>
        <Background />
        <Controls />
      </ReactFlow>
    </div>
  );
}
