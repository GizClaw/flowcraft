import { useEffect, useState } from 'react';
import { X, Trash2, ChevronDown } from 'lucide-react';
import { useSelectedNode } from '../hooks/useSelectedNode';
import { useWorkflowStore } from '../store/workflowStore';
import { toolApi, type ToolItem } from '../utils/api';

function ToolSelector({ selected, onChange }: { selected: string[]; onChange: (v: string[]) => void }) {
  const [tools, setTools] = useState<ToolItem[]>([]);
  const [loading, setLoading] = useState(true);
  const [open, setOpen] = useState(false);
  const [search, setSearch] = useState('');

  useEffect(() => {
    toolApi.list().then(setTools).finally(() => setLoading(false));
  }, []);

  const filtered = tools.filter((t) => t.name.toLowerCase().includes(search.toLowerCase()));

  const toggle = (name: string) => {
    const next = selected.includes(name) ? selected.filter((n) => n !== name) : [...selected, name];
    onChange(next);
  };

  if (loading) {
    return <div className="text-xs text-gray-400 py-1">Loading...</div>;
  }

  return (
    <div className="relative">
      <button
        type="button"
        onClick={() => setOpen(!open)}
        className="w-full flex items-center justify-between px-3 py-1.5 text-sm rounded-lg border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-800 hover:border-indigo-400 transition-colors"
      >
        <span className="text-gray-600 dark:text-gray-300 truncate">
          {selected.length ? `${selected.length} tool${selected.length > 1 ? 's' : ''} selected` : 'Select tools...'}
        </span>
        <ChevronDown size={14} className={`text-gray-400 transition-transform ${open ? 'rotate-180' : ''}`} />
      </button>

      {open && (
        <div className="absolute z-50 mt-1 w-full max-h-56 rounded-lg border border-gray-200 dark:border-gray-700 bg-white dark:bg-gray-800 shadow-lg overflow-hidden">
          <div className="p-2 border-b border-gray-100 dark:border-gray-700">
            <input
              type="text"
              value={search}
              onChange={(e) => setSearch(e.target.value)}
              placeholder="Search tools..."
              className="w-full px-2 py-1 text-xs rounded border border-gray-200 dark:border-gray-600 bg-gray-50 dark:bg-gray-900 focus:ring-1 focus:ring-indigo-500 focus:border-transparent"
              autoFocus
            />
          </div>
          <div className="overflow-y-auto max-h-44">
            {filtered.length === 0 && (
              <div className="px-3 py-2 text-xs text-gray-400">No tools found</div>
            )}
            {filtered.map((t) => (
              <label
                key={t.name}
                className="flex items-start gap-2 px-3 py-1.5 hover:bg-gray-50 dark:hover:bg-gray-700 cursor-pointer"
              >
                <input
                  type="checkbox"
                  checked={selected.includes(t.name)}
                  onChange={() => toggle(t.name)}
                  className="mt-0.5 rounded border-gray-300 text-indigo-600 focus:ring-indigo-500"
                />
                <div className="min-w-0">
                  <div className="text-xs font-medium text-gray-800 dark:text-gray-200">{t.name}</div>
                  {t.description && (
                    <div className="text-[11px] text-gray-400 truncate">{t.description}</div>
                  )}
                </div>
              </label>
            ))}
          </div>
        </div>
      )}

      {selected.length > 0 && (
        <div className="flex flex-wrap gap-1 mt-1.5">
          {selected.map((name) => (
            <span
              key={name}
              className="inline-flex items-center gap-0.5 px-1.5 py-0.5 text-[11px] font-medium rounded bg-indigo-50 dark:bg-indigo-950 text-indigo-700 dark:text-indigo-300"
            >
              {name}
              <button
                type="button"
                onClick={() => toggle(name)}
                className="hover:text-indigo-900 dark:hover:text-indigo-100"
              >
                <X size={10} />
              </button>
            </span>
          ))}
        </div>
      )}
    </div>
  );
}

export default function NodeConfigPanel() {
  const selected = useSelectedNode();
  const updateNodeConfig = useWorkflowStore((s) => s.updateNodeConfig);
  const updateNodeLabel = useWorkflowStore((s) => s.updateNodeLabel);
  const deleteNode = useWorkflowStore((s) => s.deleteNode);
  const setSelectedNodeId = useWorkflowStore((s) => s.setSelectedNodeId);

  if (!selected) return null;
  const { node, data, schema } = selected;

  const handleChange = (key: string, value: unknown) => {
    useWorkflowStore.getState().pushHistory();
    updateNodeConfig(node.id, { [key]: value });
  };

  const isLLM = data.nodeType === 'llm';
  const toolNames = (Array.isArray(data.config.tool_names) ? data.config.tool_names : []) as string[];

  return (
    <div className="w-80 border-l border-gray-200 dark:border-gray-800 bg-white dark:bg-gray-900 overflow-y-auto shrink-0">
      <div className="flex items-center justify-between p-4 border-b border-gray-200 dark:border-gray-700">
        <h3 className="text-sm font-semibold text-gray-900 dark:text-gray-100">Node Config</h3>
        <div className="flex gap-1">
          {data.nodeType !== 'start' && data.nodeType !== 'end' && (
            <button onClick={() => deleteNode(node.id)} className="p-1.5 rounded-lg hover:bg-red-50 dark:hover:bg-red-950 text-red-500" title="Delete node">
              <Trash2 size={14} />
            </button>
          )}
          <button onClick={() => setSelectedNodeId(null)} className="p-1.5 rounded-lg hover:bg-gray-100 dark:hover:bg-gray-800 text-gray-500">
            <X size={14} />
          </button>
        </div>
      </div>

      <div className="p-4 space-y-4">
        <div>
          <label className="block text-xs font-medium text-gray-500 mb-1">Label</label>
          <input
            type="text"
            value={data.label}
            onChange={(e) => updateNodeLabel(node.id, e.target.value)}
            className="w-full px-3 py-1.5 text-sm rounded-lg border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-800 focus:ring-2 focus:ring-indigo-500 focus:border-transparent"
          />
        </div>

        <div>
          <label className="block text-xs font-medium text-gray-500 mb-1">ID</label>
          <input type="text" value={node.id} readOnly className="w-full px-3 py-1.5 text-sm rounded-lg bg-gray-50 dark:bg-gray-800 border border-gray-200 dark:border-gray-700 text-gray-400" />
        </div>

        {schema?.fields?.filter((f) => !(isLLM && f.key === 'tool_names')).map((field) => (
          <div key={field.key}>
            <label className="block text-xs font-medium text-gray-500 mb-1">
              {field.label} {field.required && <span className="text-red-400">*</span>}
            </label>
            {field.type === 'textarea' ? (
              <textarea
                value={String(data.config[field.key] ?? field.default_value ?? '')}
                onChange={(e) => handleChange(field.key, e.target.value)}
                placeholder={field.placeholder}
                rows={4}
                className="w-full px-3 py-1.5 text-sm rounded-lg border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-800 focus:ring-2 focus:ring-indigo-500 focus:border-transparent resize-y"
              />
            ) : field.type === 'select' ? (
              <select
                value={String(data.config[field.key] ?? field.default_value ?? '')}
                onChange={(e) => handleChange(field.key, e.target.value)}
                className="w-full px-3 py-1.5 text-sm rounded-lg border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-800 focus:ring-2 focus:ring-indigo-500"
              >
                {field.options?.map((opt) => (
                  <option key={opt.value} value={opt.value}>{opt.label}</option>
                ))}
              </select>
            ) : field.type === 'boolean' ? (
              <label className="flex items-center gap-2 cursor-pointer">
                <input
                  type="checkbox"
                  checked={Boolean(data.config[field.key] ?? field.default_value ?? false)}
                  onChange={(e) => handleChange(field.key, e.target.checked)}
                  className="rounded border-gray-300 text-indigo-600 focus:ring-indigo-500"
                />
                <span className="text-sm text-gray-600 dark:text-gray-400">Enabled</span>
              </label>
            ) : field.type === 'number' ? (
              <input
                type="number"
                value={String(data.config[field.key] ?? field.default_value ?? '')}
                onChange={(e) => handleChange(field.key, e.target.value === '' ? undefined : Number(e.target.value))}
                placeholder={field.placeholder}
                className="w-full px-3 py-1.5 text-sm rounded-lg border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-800 focus:ring-2 focus:ring-indigo-500 focus:border-transparent"
              />
            ) : field.type === 'json' ? (
              <textarea
                value={typeof data.config[field.key] === 'string' ? (data.config[field.key] as string) : JSON.stringify(data.config[field.key] ?? field.default_value ?? '', null, 2)}
                onChange={(e) => {
                  try { handleChange(field.key, JSON.parse(e.target.value)); } catch { handleChange(field.key, e.target.value); }
                }}
                placeholder={field.placeholder}
                rows={3}
                className="w-full px-3 py-1.5 text-sm rounded-lg border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-800 font-mono focus:ring-2 focus:ring-indigo-500 focus:border-transparent resize-y"
              />
            ) : (
              <input
                type="text"
                value={String(data.config[field.key] ?? field.default_value ?? '')}
                onChange={(e) => handleChange(field.key, e.target.value)}
                placeholder={field.placeholder}
                className="w-full px-3 py-1.5 text-sm rounded-lg border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-800 focus:ring-2 focus:ring-indigo-500 focus:border-transparent"
              />
            )}
          </div>
        ))}

        {isLLM && (
          <div>
            <label className="block text-xs font-medium text-gray-500 mb-1">Tools</label>
            <ToolSelector
              selected={toolNames}
              onChange={(v) => handleChange('tool_names', v.length > 0 ? v : undefined)}
            />
          </div>
        )}
      </div>
    </div>
  );
}
