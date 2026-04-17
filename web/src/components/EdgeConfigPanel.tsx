import { X } from 'lucide-react';
import { useWorkflowStore } from '../store/workflowStore';

export default function EdgeConfigPanel() {
  const selectedEdgeId = useWorkflowStore((s) => s.selectedEdgeId);
  const edges = useWorkflowStore((s) => s.edges);
  const updateEdgeData = useWorkflowStore((s) => s.updateEdgeData);
  const setSelectedEdgeId = useWorkflowStore((s) => s.setSelectedEdgeId);

  const edge = edges.find((e) => e.id === selectedEdgeId);
  if (!edge) return null;

  const condition = (edge.data as Record<string, unknown>)?.condition as string || '';

  return (
    <div className="w-72 border-l border-gray-200 dark:border-gray-800 bg-white dark:bg-gray-900 overflow-y-auto shrink-0">
      <div className="flex items-center justify-between p-4 border-b border-gray-200 dark:border-gray-700">
        <h3 className="text-sm font-semibold text-gray-900 dark:text-gray-100">Edge Config</h3>
        <button onClick={() => setSelectedEdgeId(null)} className="p-1.5 rounded-lg hover:bg-gray-100 dark:hover:bg-gray-800 text-gray-500">
          <X size={14} />
        </button>
      </div>
      <div className="p-4 space-y-4">
        <div>
          <label className="block text-xs font-medium text-gray-500 mb-1">From → To</label>
          <p className="text-sm text-gray-700 dark:text-gray-300">{edge.source} → {edge.target}</p>
        </div>
        <div>
          <label className="block text-xs font-medium text-gray-500 mb-1">Condition (expr-lang)</label>
          <textarea
            value={condition}
            onChange={(e) => {
              useWorkflowStore.getState().pushHistory();
              updateEdgeData(edge.id, { condition: e.target.value });
            }}
            placeholder="e.g. tool_pending == true"
            rows={3}
            className="w-full px-3 py-1.5 text-sm rounded-lg border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-800 font-mono focus:ring-2 focus:ring-indigo-500 focus:border-transparent resize-y"
          />
          <p className="text-[10px] text-gray-400 mt-1">Leave empty for unconditional edge</p>
        </div>
      </div>
    </div>
  );
}
