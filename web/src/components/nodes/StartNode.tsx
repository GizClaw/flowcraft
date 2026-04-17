import { Handle, Position } from '@xyflow/react';
import { Play } from 'lucide-react';

export default function StartNode() {
  return (
    <div className="px-4 py-2 rounded-lg bg-indigo-50 dark:bg-indigo-950 border-2 border-indigo-400 shadow-sm">
      <div className="flex items-center gap-2 text-indigo-700 dark:text-indigo-300">
        <Play size={14} />
        <span className="text-sm font-medium">Start</span>
      </div>
      <Handle type="source" position={Position.Bottom} className="!bg-indigo-500 !w-3 !h-3" />
    </div>
  );
}
