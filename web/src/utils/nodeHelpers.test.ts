import { describe, it, expect } from 'vitest';
import { toGraphDefinition, graphDefToReactFlow, detectParallelSets, graphDefToYaml, parseYamlGraphDef } from './nodeHelpers';
import type { Node, Edge } from '@xyflow/react';
import type { GraphDefinition } from '../types/app';

// ── helpers ──

function makeNode(id: string, nodeType: string, opts: { label?: string; config?: Record<string, unknown>; position?: { x: number; y: number } } = {}): Node {
  return {
    id,
    type: nodeType === 'start' ? 'start' : nodeType === 'end' ? 'end' : 'custom',
    data: {
      label: opts.label ?? `${nodeType} Node`,
      nodeType,
      config: opts.config ?? {},
    },
    position: opts.position ?? { x: 100, y: 100 },
  };
}

function makeEdge(source: string, target: string, opts: { condition?: string } = {}): Edge {
  return {
    id: `e-${source}-${target}`,
    source,
    target,
    data: opts.condition ? { condition: opts.condition } : {},
  };
}

// ── toGraphDefinition ──

describe('toGraphDefinition', () => {
  it('converts ReactFlow nodes/edges to GraphDefinition', () => {
    const nodes = [
      makeNode('__start__', 'start'),
      makeNode('llm-1', 'llm', { label: 'GPT', config: { model: 'gpt-4' } }),
      makeNode('__end__', 'end'),
    ];
    const edges = [
      makeEdge('__start__', 'llm-1'),
      makeEdge('llm-1', '__end__'),
    ];

    const def = toGraphDefinition(nodes, edges, 'test-flow');

    expect(def.name).toBe('test-flow');
    expect(def.entry).toBe('llm-1');
    expect(def.nodes).toHaveLength(1);
    expect(def.nodes[0].id).toBe('llm-1');
    expect(def.nodes[0].type).toBe('llm');
    expect(def.nodes[0].config?.model).toBe('gpt-4');
    expect(def.nodes[0].config?.__position).toEqual({ x: 100, y: 100 });
    expect(def.nodes[0].config?.__label).toBe('GPT');
  });

  it('excludes start and end nodes from node list', () => {
    const nodes = [
      makeNode('__start__', 'start'),
      makeNode('a', 'llm'),
      makeNode('b', 'template'),
      makeNode('__end__', 'end'),
    ];
    const edges = [
      makeEdge('__start__', 'a'),
      makeEdge('a', 'b'),
      makeEdge('b', '__end__'),
    ];

    const def = toGraphDefinition(nodes, edges);
    expect(def.nodes).toHaveLength(2);
    expect(def.nodes.map(n => n.id)).toEqual(['a', 'b']);
  });

  it('preserves edge conditions', () => {
    const nodes = [
      makeNode('__start__', 'start'),
      makeNode('router', 'router'),
      makeNode('a', 'llm'),
      makeNode('b', 'llm'),
    ];
    const edges = [
      makeEdge('__start__', 'router'),
      makeEdge('router', 'a', { condition: 'category == "A"' }),
      makeEdge('router', 'b', { condition: 'category == "B"' }),
    ];

    const def = toGraphDefinition(nodes, edges);
    expect(def.edges).toHaveLength(2);
    expect(def.edges[0].condition).toBe('category == "A"');
    expect(def.edges[1].condition).toBe('category == "B"');
  });

  it('handles graph with no start node', () => {
    const nodes = [makeNode('a', 'llm')];
    const edges: Edge[] = [];

    const def = toGraphDefinition(nodes, edges);
    expect(def.entry).toBe('');
    expect(def.nodes).toHaveLength(1);
  });
});

// ── graphDefToReactFlow ──

describe('graphDefToReactFlow', () => {
  it('converts GraphDefinition to ReactFlow format', () => {
    const def: GraphDefinition = {
      name: 'workflow',
      entry: 'llm-1',
      nodes: [
        { id: 'llm-1', type: 'llm', config: { model: 'gpt-4', __position: { x: 200, y: 200 }, __label: 'GPT' } },
      ],
      edges: [
        { from: 'llm-1', to: '__end__' },
      ],
    };

    const { nodes } = graphDefToReactFlow(def);

    // start + llm-1 + __end__ (auto-created)
    expect(nodes).toHaveLength(3);
    expect(nodes[0].id).toBe('__start__');
    expect(nodes[1].id).toBe('llm-1');
    expect(nodes[1].position).toEqual({ x: 200, y: 200 });
    expect((nodes[1].data as Record<string, unknown>).label).toBe('GPT');
    expect((nodes[1].data as Record<string, unknown>).nodeType).toBe('llm');
    // __position and __label stripped from config
    const config = (nodes[1].data as Record<string, unknown>).config as Record<string, unknown>;
    expect(config.__position).toBeUndefined();
    expect(config.__label).toBeUndefined();
    expect(config.model).toBe('gpt-4');

    expect(nodes[2].id).toBe('__end__');
  });

  it('creates entry edge from __start__ to entry node', () => {
    const def: GraphDefinition = {
      name: 'w',
      entry: 'n1',
      nodes: [{ id: 'n1', type: 'llm' }],
      edges: [],
    };

    const { edges } = graphDefToReactFlow(def);
    expect(edges[0].source).toBe('__start__');
    expect(edges[0].target).toBe('n1');
  });

  it('handles empty graph', () => {
    const def: GraphDefinition = { name: 'empty', entry: '', nodes: [], edges: [] };
    const { nodes, edges } = graphDefToReactFlow(def);

    expect(nodes).toHaveLength(1); // only __start__
    expect(edges).toHaveLength(0);
  });

  it('auto-creates __end__ node when edge references it', () => {
    const def: GraphDefinition = {
      name: 'w',
      entry: 'n1',
      nodes: [{ id: 'n1', type: 'llm' }],
      edges: [{ from: 'n1', to: '__end__' }],
    };

    const { nodes } = graphDefToReactFlow(def);
    const endNode = nodes.find(n => n.id === '__end__');
    expect(endNode).toBeTruthy();
    expect(endNode!.type).toBe('end');
  });

  it('assigns default position if __position not provided', () => {
    const def: GraphDefinition = {
      name: 'w',
      entry: 'n1',
      nodes: [{ id: 'n1', type: 'llm' }],
      edges: [],
    };

    const { nodes } = graphDefToReactFlow(def);
    expect(Number.isFinite(nodes[1].position.x)).toBe(true);
    expect(Number.isFinite(nodes[1].position.y)).toBe(true);
  });
});

// ── YAML roundtrip ──

describe('graphDefToYaml / parseYamlGraphDef', () => {
  it('roundtrips a graph definition through YAML', () => {
    const def: GraphDefinition = {
      name: 'test',
      entry: 'llm-1',
      nodes: [{ id: 'llm-1', type: 'llm', config: { model: 'gpt-4' } }],
      edges: [{ from: 'llm-1', to: '__end__' }],
    };

    const yamlStr = graphDefToYaml(def);
    const parsed = parseYamlGraphDef(yamlStr);

    expect(parsed.name).toBe('test');
    expect(parsed.entry).toBe('llm-1');
    expect(parsed.nodes).toHaveLength(1);
    expect(parsed.edges).toHaveLength(1);
  });
});

// ── detectParallelSets ──

describe('detectParallelSets', () => {
  it('detects fork edges and join nodes', () => {
    const nodes = [
      makeNode('a', 'llm'),
      makeNode('b', 'llm'),
      makeNode('c', 'llm'),
      makeNode('d', 'llm'),
    ];
    // a → b, a → c (fork), b → d, c → d (join at d)
    const edges = [
      makeEdge('a', 'b'),
      makeEdge('a', 'c'),
      makeEdge('b', 'd'),
      makeEdge('c', 'd'),
    ];

    const { forkEdgeIds, joinNodeIds } = detectParallelSets(nodes, edges);

    expect(forkEdgeIds.size).toBe(2);
    expect(forkEdgeIds.has('e-a-b')).toBe(true);
    expect(forkEdgeIds.has('e-a-c')).toBe(true);
    expect(joinNodeIds.size).toBe(1);
    expect(joinNodeIds.has('d')).toBe(true);
  });

  it('returns empty sets for linear graph', () => {
    const nodes = [makeNode('a', 'llm'), makeNode('b', 'llm')];
    const edges = [makeEdge('a', 'b')];

    const { forkEdgeIds, joinNodeIds } = detectParallelSets(nodes, edges);
    expect(forkEdgeIds.size).toBe(0);
    expect(joinNodeIds.size).toBe(0);
  });

  it('handles empty graph', () => {
    const { forkEdgeIds, joinNodeIds } = detectParallelSets([], []);
    expect(forkEdgeIds.size).toBe(0);
    expect(joinNodeIds.size).toBe(0);
  });
});
