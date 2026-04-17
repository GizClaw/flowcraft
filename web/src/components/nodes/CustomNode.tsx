import { Handle, Position, type NodeProps } from '@xyflow/react';
import * as icons from 'lucide-react';
import { useNodeTypeStore } from '../../store/nodeTypeStore';
import { useWorkflowStore } from '../../store/workflowStore';
import { deriveColorClasses } from '../../types/nodeTypes';
import { STATUS_COLORS } from '../../constants/colors';
import { NODE_ICONS } from '../../constants/icons';
import type { CustomNodeData } from '../../store/workflowStore';

export default function CustomNode({ id, data, selected }: NodeProps) {
  const d = data as CustomNodeData;
  const byType = useNodeTypeStore((s) => s.byType);
  const schema = byType[d.nodeType];
  const nodeStatus = useWorkflowStore((s) => s.nodeStatuses.get(id));
  const warnings = useWorkflowStore((s) => s.nodeWarnings.get(id));
  const errors = useWorkflowStore((s) => s.nodeErrors.get(id));

  const color = schema?.color ? deriveColorClasses(schema.color) : deriveColorClasses('gray');
  const iconName = schema?.icon || NODE_ICONS[d.nodeType] || 'Circle';
  const Icon = (icons as unknown as Record<string, React.FC<{ size?: number; className?: string }>>)[iconName] || icons.Circle;
  const statusClass = nodeStatus ? STATUS_COLORS[nodeStatus] || '' : '';
  const isRunning = nodeStatus === 'running';

  return (
    <div className={`px-4 py-2.5 rounded-lg ${color.bg} ${color.darkBg} border-2 ${color.border} shadow-sm min-w-[140px] ${selected ? 'ring-2 ring-indigo-500 ring-offset-1' : ''} ${statusClass} ${isRunning ? 'node-running' : ''}`}>
      <Handle type="target" position={Position.Top} className="!bg-gray-400 !w-3 !h-3" />
      <div className="flex items-center gap-2">
        <Icon size={16} className={color.text} />
        <span className={`text-sm font-medium ${color.text} truncate max-w-[120px]`}>{d.label}</span>
        {errors && errors.length > 0 && (
          <span className="w-4 h-4 rounded-full bg-red-500 text-white text-[10px] flex items-center justify-center shrink-0" title={errors.join('\n')}>!</span>
        )}
        {warnings && warnings.length > 0 && !errors?.length && (
          <span className="w-4 h-4 rounded-full bg-amber-500 text-white text-[10px] flex items-center justify-center shrink-0" title={warnings.join('\n')}>!</span>
        )}
      </div>
      {schema && (
        <div className="flex items-center gap-1 mt-0.5">
          <p className="text-[10px] text-gray-400 dark:text-gray-500 truncate">{schema.type}</p>
          {schema.category === 'plugin' && (
            <span className="text-[8px] font-semibold px-1 py-px rounded bg-violet-100 text-violet-600 dark:bg-violet-900 dark:text-violet-300 shrink-0">Plugin</span>
          )}
        </div>
      )}
      <Handle type="source" position={Position.Bottom} className="!bg-gray-400 !w-3 !h-3" />
    </div>
  );
}
