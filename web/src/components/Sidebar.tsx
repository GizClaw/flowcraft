import { useMemo } from 'react';
import { useTranslation } from 'react-i18next';
import * as icons from 'lucide-react';
import { useNodeTypeStore } from '../store/nodeTypeStore';
import { deriveColorClasses } from '../types/nodeTypes';
import type { NodeSchema } from '../types/nodeTypes';

export default function Sidebar() {
  const { t } = useTranslation();
  const schemas = useNodeTypeStore((s) => s.schemas);
  const loading = useNodeTypeStore((s) => s.loading);
  const error = useNodeTypeStore((s) => s.error);

  const { categories, schemasByCategory } = useMemo(() => {
    const cats = [...new Set(schemas.map((s) => s.category))];
    const byCat: Record<string, NodeSchema[]> = {};
    for (const cat of cats) {
      byCat[cat] = schemas.filter((s) => s.category === cat && !s.deprecated);
    }
    return { categories: cats, schemasByCategory: byCat };
  }, [schemas]);

  const onDragStart = (event: React.DragEvent, nodeType: string) => {
    event.dataTransfer.setData('application/reactflow', nodeType);
    event.dataTransfer.effectAllowed = 'move';
  };

  return (
    <div className="w-56 border-r border-gray-200 dark:border-gray-800 bg-white dark:bg-gray-900 overflow-y-auto shrink-0">
      <div className="p-3">
        <h3 className="text-xs font-semibold text-gray-400 uppercase tracking-wider mb-3">{t('editor.nodes')}</h3>
        {loading && <div className="text-xs text-gray-400">{t('common.loading')}</div>}
        {error && <div className="text-xs text-red-500">{error}</div>}
        {!loading && !error && categories.map((cat) => {
          const catSchemas = schemasByCategory[cat];
          if (!catSchemas || catSchemas.length === 0) return null;
          return (
            <div key={cat} className="mb-4">
              <h4 className="text-[11px] font-medium text-gray-500 dark:text-gray-400 uppercase mb-1.5 px-1">{cat}</h4>
              <div className="space-y-1">
                {catSchemas.map((schema) => (
                  <NodeItem key={schema.type} schema={schema} onDragStart={onDragStart} />
                ))}
              </div>
            </div>
          );
        })}
      </div>
    </div>
  );
}

function NodeItem({ schema, onDragStart }: { schema: NodeSchema; onDragStart: (e: React.DragEvent, type: string) => void }) {
  const color = deriveColorClasses(schema.color);
  const Icon = (icons as unknown as Record<string, React.FC<{ size?: number; className?: string }>>)[schema.icon] || icons.Circle;

  return (
    <div
      draggable
      onDragStart={(e) => onDragStart(e, schema.type)}
      className={`flex items-center gap-2 px-2.5 py-2 rounded-lg cursor-grab active:cursor-grabbing ${color.bg} ${color.darkBg} border border-transparent hover:border-gray-300 dark:hover:border-gray-600 transition-colors`}
    >
      <Icon size={14} className={color.text} />
      <span className="text-xs font-medium text-gray-700 dark:text-gray-300 truncate">{schema.label}</span>
      {schema.category === 'plugin' && (
        <span className="ml-auto text-[9px] font-semibold px-1.5 py-0.5 rounded bg-violet-100 text-violet-600 dark:bg-violet-900 dark:text-violet-300 shrink-0">Plugin</span>
      )}
    </div>
  );
}
