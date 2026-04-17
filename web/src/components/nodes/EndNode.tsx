import { Handle, Position } from '@xyflow/react';
import { CircleStop } from 'lucide-react';

export default function EndNode() {
  return (
    <div className="px-4 py-2 rounded-lg bg-red-50 dark:bg-red-950 border-2 border-red-400 shadow-sm">
      <Handle type="target" position={Position.Top} className="!bg-red-500 !w-3 !h-3" />
      <div className="flex items-center gap-2 text-red-700 dark:text-red-300">
        <CircleStop size={14} />
        <span className="text-sm font-medium">End</span>
      </div>
    </div>
  );
}
