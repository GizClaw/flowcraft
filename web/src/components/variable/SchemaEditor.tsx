import { useState } from 'react';
import { Plus, Trash2 } from 'lucide-react';
import type { Variable, VariableSchema, VariableType } from '../../types/variable';

const TYPES: VariableType[] = ['string', 'number', 'integer', 'boolean', 'array', 'object', 'file', 'any'];

interface Props {
  schema: VariableSchema;
  onChange: (schema: VariableSchema) => void;
}

export default function SchemaEditor({ schema, onChange }: Props) {
  const [newName, setNewName] = useState('');

  const addVariable = () => {
    if (!newName.trim()) return;
    const v: Variable = { name: newName.trim(), type: 'string' };
    onChange({ variables: [...schema.variables, v] });
    setNewName('');
  };

  const updateVariable = (index: number, updates: Partial<Variable>) => {
    const next = schema.variables.map((v, i) => (i === index ? { ...v, ...updates } : v));
    onChange({ variables: next });
  };

  const removeVariable = (index: number) => {
    onChange({ variables: schema.variables.filter((_, i) => i !== index) });
  };

  return (
    <div className="space-y-3">
      {schema.variables.map((v, i) => (
        <div key={i} className="flex items-start gap-2 p-3 bg-gray-50 dark:bg-gray-800 rounded-lg">
          <div className="flex-1 space-y-2">
            <div className="flex gap-2">
              <input
                value={v.name}
                onChange={(e) => updateVariable(i, { name: e.target.value })}
                placeholder="Name"
                className="flex-1 px-2 py-1 text-sm rounded border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-700"
              />
              <select
                value={v.type}
                onChange={(e) => updateVariable(i, { type: e.target.value as VariableType })}
                className="px-2 py-1 text-sm rounded border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-700"
              >
                {TYPES.map((t) => <option key={t} value={t}>{t}</option>)}
              </select>
            </div>
            <div className="flex gap-2 items-center">
              <label className="flex items-center gap-1 text-xs text-gray-500">
                <input type="checkbox" checked={v.required || false} onChange={(e) => updateVariable(i, { required: e.target.checked })} className="rounded text-indigo-600" />
                Required
              </label>
              <input
                value={v.description || ''}
                onChange={(e) => updateVariable(i, { description: e.target.value })}
                placeholder="Description"
                className="flex-1 px-2 py-1 text-xs rounded border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-700"
              />
            </div>
          </div>
          <button onClick={() => removeVariable(i)} className="p-1 text-red-400 hover:text-red-600 mt-1">
            <Trash2 size={14} />
          </button>
        </div>
      ))}

      <div className="flex gap-2">
        <input
          value={newName}
          onChange={(e) => setNewName(e.target.value)}
          onKeyDown={(e) => e.key === 'Enter' && addVariable()}
          placeholder="Variable name"
          className="flex-1 px-3 py-1.5 text-sm rounded-lg border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-800"
        />
        <button onClick={addVariable} className="flex items-center gap-1 px-3 py-1.5 text-sm bg-indigo-600 text-white rounded-lg hover:bg-indigo-700">
          <Plus size={14} /> Add
        </button>
      </div>
    </div>
  );
}
