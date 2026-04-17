import { useState, useRef, useEffect } from 'react';
import type { VariableScope } from '../../types/variable';

interface Props {
  value: string;
  onChange: (value: string) => void;
  placeholder?: string;
  availableScopes?: Record<VariableScope, string[]>;
}

export default function VariableRefInput({ value, onChange, placeholder, availableScopes }: Props) {
  const [showPopover, setShowPopover] = useState(false);
  const [filter, setFilter] = useState('');
  const inputRef = useRef<HTMLTextAreaElement>(null);

  const allRefs: { ref: string; scope: string; name: string }[] = [];
  if (availableScopes) {
    for (const [scope, vars] of Object.entries(availableScopes)) {
      for (const v of vars) {
        const ref = `\${${scope}.${v}}`;
        allRefs.push({ ref, scope, name: v });
      }
    }
  }

  const filtered = filter ? allRefs.filter((r) => r.ref.includes(filter) || r.name.includes(filter)) : allRefs;

  const insertRef = (ref: string) => {
    const el = inputRef.current;
    if (el) {
      const start = el.selectionStart;
      const end = el.selectionEnd;
      const newVal = value.slice(0, start) + ref + value.slice(end);
      onChange(newVal);
      setShowPopover(false);
      setTimeout(() => {
        el.selectionStart = el.selectionEnd = start + ref.length;
        el.focus();
      }, 0);
    } else {
      onChange(value + ref);
      setShowPopover(false);
    }
  };

  useEffect(() => {
    if (!showPopover) setFilter('');
  }, [showPopover]);

  return (
    <div className="relative">
      <textarea
        ref={inputRef}
        value={value}
        onChange={(e) => onChange(e.target.value)}
        onKeyDown={(e) => {
          if (e.key === '$' && availableScopes) setShowPopover(true);
        }}
        placeholder={placeholder}
        rows={3}
        className="w-full px-3 py-1.5 text-sm rounded-lg border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-800 font-mono focus:ring-2 focus:ring-indigo-500 focus:border-transparent resize-y"
      />
      {availableScopes && (
        <button
          type="button"
          onClick={() => setShowPopover(!showPopover)}
          className="absolute top-1.5 right-1.5 px-1.5 py-0.5 text-[10px] bg-gray-100 dark:bg-gray-700 rounded text-gray-500 hover:bg-gray-200 dark:hover:bg-gray-600"
        >
          {'${...}'}
        </button>
      )}
      {showPopover && filtered.length > 0 && (
        <div className="absolute z-10 top-full mt-1 w-full bg-white dark:bg-gray-800 border border-gray-200 dark:border-gray-600 rounded-lg shadow-lg max-h-48 overflow-y-auto">
          <input
            value={filter}
            onChange={(e) => setFilter(e.target.value)}
            placeholder="Filter..."
            className="w-full px-3 py-1.5 text-xs border-b border-gray-200 dark:border-gray-700 bg-transparent"
            autoFocus
          />
          {filtered.map((r) => (
            <button
              key={r.ref}
              onClick={() => insertRef(r.ref)}
              className="w-full text-left px-3 py-1.5 text-xs hover:bg-gray-100 dark:hover:bg-gray-700 flex items-center gap-2"
            >
              <span className="text-indigo-500 font-mono">{r.ref}</span>
              <span className="text-gray-400">{r.scope}</span>
            </button>
          ))}
        </div>
      )}
    </div>
  );
}
