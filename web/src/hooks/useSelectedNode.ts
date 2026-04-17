import { useMemo } from 'react';
import { useWorkflowStore, type CustomNodeData } from '../store/workflowStore';
import { useNodeTypeStore } from '../store/nodeTypeStore';

export function useSelectedNode() {
  const selectedNodeId = useWorkflowStore((s) => s.selectedNodeId);
  const nodes = useWorkflowStore((s) => s.nodes);
  const schemas = useNodeTypeStore((s) => s.schemas);

  return useMemo(() => {
    if (!selectedNodeId) return null;
    const node = nodes.find((n) => n.id === selectedNodeId);
    if (!node) return null;
    const data = node.data as CustomNodeData;
    const schema = schemas.find((s) => s.type === data.nodeType);
    return { node, data, schema };
  }, [selectedNodeId, nodes, schemas]);
}
