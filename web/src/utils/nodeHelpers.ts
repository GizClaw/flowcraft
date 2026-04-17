import type { Node, Edge } from '@xyflow/react';
import type { GraphDefinition, NodeDefinition, EdgeDefinition } from '../types/app';
import type { CustomNodeData } from '../store/workflowStore';
import Dagre from '@dagrejs/dagre';
import * as yaml from 'js-yaml';

export function toGraphDefinition(nodes: Node[], edges: Edge[], name = 'workflow'): GraphDefinition {
  const startNode = nodes.find((n) => (n.data as CustomNodeData).nodeType === 'start');
  const endNode = nodes.find((n) => (n.data as CustomNodeData).nodeType === 'end');
  const startId = startNode?.id || '__start__';

  const outFromStart = edges.filter((e) => e.source === startId);
  const entry = outFromStart.length > 0 ? outFromStart[0].target : '';

  const graphNodes: NodeDefinition[] = nodes
    .filter((n) => {
      const nt = (n.data as CustomNodeData).nodeType;
      return nt !== 'start' && nt !== 'end';
    })
    .map((n) => {
      const data = n.data as CustomNodeData;
      const def: NodeDefinition = { id: n.id, type: data.nodeType };
      if (data.config && Object.keys(data.config).length > 0) {
        def.config = { ...data.config, __position: n.position, __label: data.label };
      } else {
        def.config = { __position: n.position, __label: data.label };
      }
      return def;
    });

  // Persist Start/End positions into the entry node's config so they
  // survive the Go backend's typed GraphDefinition round-trip.
  const entryNodeDef = graphNodes.find((n) => n.id === entry);
  if (entryNodeDef?.config) {
    if (startNode) entryNodeDef.config.__start_position = startNode.position;
    if (endNode) entryNodeDef.config.__end_position = endNode.position;
  }

  const graphEdges: EdgeDefinition[] = edges
    .filter((e) => e.source !== startId)
    .map((e) => {
      const to = e.target === '__end__' ? '__end__' : e.target;
      const def: EdgeDefinition = { from: e.source, to };
      const cond = (e.data as Record<string, unknown>)?.condition as string | undefined;
      if (cond) def.condition = cond;
      return def;
    });

  return { name, entry, nodes: graphNodes, edges: graphEdges };
}

export function graphDefToReactFlow(def: GraphDefinition): { nodes: Node[]; edges: Edge[] } {
  const defNodes = def.nodes ?? [];
  const defEdges = def.edges ?? [];

  const entryNodeDef = defNodes.find((n) => n.id === def.entry);
  const savedStartPos = entryNodeDef?.config?.__start_position as { x: number; y: number } | undefined;
  const savedEndPos = entryNodeDef?.config?.__end_position as { x: number; y: number } | undefined;

  const nodes: Node[] = [
    {
      id: '__start__',
      type: 'start',
      data: { label: 'Start', nodeType: 'start', config: {} },
      position: savedStartPos || { x: 250, y: 50 },
    },
  ];

  let hasAnyPosition = !!(savedStartPos || savedEndPos);
  let yOffset = 150;
  for (const nd of defNodes) {
    const savedPos = nd.config?.__position as { x: number; y: number } | undefined;
    if (savedPos) hasAnyPosition = true;
    const pos = savedPos || { x: 250, y: yOffset };
    const label = (nd.config?.__label as string) || `${nd.type} Node`;
    const config = { ...nd.config };
    delete config.__position;
    delete config.__label;
    delete config.__start_position;
    delete config.__end_position;

    nodes.push({
      id: nd.id,
      type: nd.type === 'end' ? 'end' : 'custom',
      data: { label, nodeType: nd.type, config },
      position: pos,
    });
    yOffset += 120;
  }

  const hasEnd = defNodes.some((n) => n.type === 'end') || defEdges.some((e) => e.to === '__end__');
  if (hasEnd && !defNodes.some((n) => n.id === '__end__')) {
    nodes.push({
      id: '__end__',
      type: 'end',
      data: { label: 'End', nodeType: 'end', config: {} },
      position: savedEndPos || { x: 250, y: yOffset },
    });
  }

  const edges: Edge[] = defEdges.map((ed, i) => ({
    id: `edge-${ed.from}-${ed.to}-${i}`,
    source: ed.from,
    target: ed.to,
    type: 'smoothstep',
    data: ed.condition ? { condition: ed.condition } : {},
    label: ed.condition || undefined,
  }));

  if (def.entry) {
    edges.unshift({
      id: `edge-__start__-${def.entry}`,
      source: '__start__',
      target: def.entry,
      type: 'smoothstep',
      data: {},
    });
  }

  const layoutNodes = hasAnyPosition ? nodes : applyDagreLayout(nodes, edges);
  return { nodes: layoutNodes, edges };
}

export function graphDefToYaml(def: GraphDefinition): string {
  return yaml.dump(def, { lineWidth: 120, noRefs: true });
}

export function parseYamlGraphDef(yamlStr: string): GraphDefinition {
  return yaml.load(yamlStr) as GraphDefinition;
}

export function detectParallelSets(nodes: Node[], edges: Edge[]): { forkEdgeIds: Set<string>; joinNodeIds: Set<string> } {
  const forkEdgeIds = new Set<string>();
  const joinNodeIds = new Set<string>();

  const outMap = new Map<string, Edge[]>();
  const inMap = new Map<string, Edge[]>();
  for (const e of edges) {
    outMap.set(e.source, [...(outMap.get(e.source) || []), e]);
    inMap.set(e.target, [...(inMap.get(e.target) || []), e]);
  }

  for (const node of nodes) {
    const outEdges = outMap.get(node.id) || [];
    if (outEdges.length > 1) {
      outEdges.forEach((e) => forkEdgeIds.add(e.id));
    }
    const inEdges = inMap.get(node.id) || [];
    if (inEdges.length > 1) {
      joinNodeIds.add(node.id);
    }
  }

  return { forkEdgeIds, joinNodeIds };
}

const LAYOUT_NODE_WIDTH = 180;
const LAYOUT_NODE_HEIGHT = 40;

export function applyDagreLayout(
  nodes: Node[],
  edges: Edge[],
  direction: 'TB' | 'LR' = 'TB',
): Node[] {
  const g = new Dagre.graphlib.Graph().setDefaultEdgeLabel(() => ({}));
  g.setGraph({ rankdir: direction, ranksep: 80, nodesep: 50 });

  nodes.forEach((n) => g.setNode(n.id, { width: LAYOUT_NODE_WIDTH, height: LAYOUT_NODE_HEIGHT }));
  edges.forEach((e) => g.setEdge(e.source, e.target));

  Dagre.layout(g);

  return nodes.map((n) => {
    const pos = g.node(n.id);
    return { ...n, position: { x: pos.x - LAYOUT_NODE_WIDTH / 2, y: pos.y - LAYOUT_NODE_HEIGHT / 2 } };
  });
}
