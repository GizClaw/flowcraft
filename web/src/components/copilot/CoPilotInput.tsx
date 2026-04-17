import { useState, useRef, useEffect, useMemo } from 'react';
import { useTranslation } from 'react-i18next';
import { Send, Square } from 'lucide-react';
import { useWorkflowStore, type CustomNodeData } from '../../store/workflowStore';

interface Props {
  onSend: (message: string) => void;
  onStop?: () => void;
  isStreaming?: boolean;
}

interface NodeCandidate {
  id: string;
  label: string;
  nodeType: string;
}

export default function CoPilotInput({ onSend, onStop, isStreaming }: Props) {
  const { t } = useTranslation();
  const [input, setInput] = useState('');
  const [showMentionMenu, setShowMentionMenu] = useState(false);
  const [mentionSearch, setMentionSearch] = useState('');
  const [selectedIndex, setSelectedIndex] = useState(0);
  const inputRef = useRef<HTMLTextAreaElement>(null);
  const composingRef = useRef(false);
  const justComposedRef = useRef(false);

  const nodes = useWorkflowStore((s) => s.nodes);

  const filteredNodes = useMemo<NodeCandidate[]>(() => {
    const candidates = nodes
      .filter((n) => n.id !== '__start__' && n.id !== '__end__')
      .map((n) => {
        const data = n.data as CustomNodeData;
        return { id: n.id, label: data.label || n.id, nodeType: data.nodeType || 'unknown' };
      });

    if (!mentionSearch) return candidates;
    const lower = mentionSearch.toLowerCase();
    return candidates.filter((c) => c.label.toLowerCase().includes(lower) || c.id.toLowerCase().includes(lower));
  }, [nodes, mentionSearch]);

  const handleSubmit = () => {
    const msg = input.trim();
    if (!msg || isStreaming) return;
    onSend(msg);
    setInput('');
  };

  const detectMention = (value: string, cursorPos: number): boolean => {
    if (cursorPos === 0) return false;
    const beforeCursor = value.slice(0, cursorPos);
    const lastAt = beforeCursor.lastIndexOf('@');
    if (lastAt === -1) return false;
    const afterAt = beforeCursor.slice(lastAt + 1);
    if (afterAt.includes(' ')) return false;
    if (afterAt !== mentionSearch) {
      setMentionSearch(afterAt);
      setSelectedIndex(0);
    }
    return true;
  };

  const insertMention = (node: NodeCandidate) => {
    const inputEl = inputRef.current;
    if (!inputEl) return;

    const cursorPos = inputEl.selectionStart;
    const beforeCursor = input.slice(0, cursorPos);
    const lastAt = beforeCursor.lastIndexOf('@');
    const afterCursor = input.slice(cursorPos);

    const newValue = beforeCursor.slice(0, lastAt) + `[ref:node:${node.id}]` + afterCursor;
    setInput(newValue);
    setShowMentionMenu(false);
    setMentionSearch('');

    setTimeout(() => {
      const newPos = lastAt + `[ref:node:${node.id}]`.length;
      inputEl.setSelectionRange(newPos, newPos);
      inputEl.focus();
    }, 0);
  };

  const handleChange = (e: React.ChangeEvent<HTMLTextAreaElement>) => {
    const value = e.target.value;
    const cursorPos = e.target.selectionStart;
    setInput(value);
    setShowMentionMenu(detectMention(value, cursorPos));
  };

  const handleKeyDown = (e: React.KeyboardEvent) => {
    if (showMentionMenu && filteredNodes.length > 0) {
      if (e.key === 'ArrowDown') {
        e.preventDefault();
        setSelectedIndex((prev) => (prev + 1) % filteredNodes.length);
        return;
      }
      if (e.key === 'ArrowUp') {
        e.preventDefault();
        setSelectedIndex((prev) => (prev - 1 + filteredNodes.length) % filteredNodes.length);
        return;
      }
      if (e.key === 'Escape') {
        e.preventDefault();
        setShowMentionMenu(false);
        return;
      }
    }

    if (e.key === 'Enter' && !e.shiftKey) {
      if (composingRef.current || e.nativeEvent.isComposing || e.keyCode === 229) {
        e.preventDefault();
        return;
      }
      if (justComposedRef.current) {
        justComposedRef.current = false;
        e.preventDefault();
        return;
      }
      if (showMentionMenu && filteredNodes.length > 0) {
        e.preventDefault();
        insertMention(filteredNodes[selectedIndex]);
        return;
      }
      e.preventDefault();
      handleSubmit();
    }
  };

  useEffect(() => {
    const handleClickOutside = () => setShowMentionMenu(false);
    if (showMentionMenu) {
      document.addEventListener('click', handleClickOutside);
      return () => document.removeEventListener('click', handleClickOutside);
    }
  }, [showMentionMenu]);

  return (
    <div className="relative flex items-end gap-2 p-3 border-t border-gray-200 dark:border-gray-800">
      {showMentionMenu && filteredNodes.length > 0 && (
        <div
          className="absolute bottom-full left-0 mb-2 w-64 max-h-48 overflow-y-auto bg-white dark:bg-gray-800 border border-gray-200 dark:border-gray-700 rounded-lg shadow-lg z-50"
        >
          {filteredNodes.map((node, idx) => (
            <button
              key={node.id}
              onClick={() => insertMention(node)}
              className={`w-full px-3 py-2 text-left text-sm flex items-center justify-between ${
                idx === selectedIndex
                  ? 'bg-indigo-50 dark:bg-indigo-900/30'
                  : 'hover:bg-gray-100 dark:hover:bg-gray-700'
              }`}
            >
              <span className="font-medium">{node.label}</span>
              <span className="text-xs text-gray-500">{node.nodeType}</span>
            </button>
          ))}
        </div>
      )}
      <textarea
        ref={inputRef}
        value={input}
        onChange={handleChange}
        onCompositionStart={() => { composingRef.current = true; }}
        onCompositionEnd={() => { composingRef.current = false; justComposedRef.current = true; }}
        onKeyDown={handleKeyDown}
        onKeyUp={() => { justComposedRef.current = false; }}
        placeholder={t('copilot.inputPlaceholder')}
        rows={1}
        className="flex-1 px-3 py-2 text-sm rounded-xl border border-gray-300 dark:border-gray-600 bg-gray-50 dark:bg-gray-800 focus:ring-2 focus:ring-indigo-500 focus:border-transparent resize-none max-h-24"
      />
      {isStreaming ? (
        <button onClick={onStop} className="p-2 rounded-xl bg-red-500 text-white hover:bg-red-600 shrink-0">
          <Square size={16} />
        </button>
      ) : (
        <button onClick={handleSubmit} disabled={!input.trim()} className="p-2 rounded-xl bg-indigo-600 text-white hover:bg-indigo-700 disabled:opacity-50 shrink-0">
          <Send size={16} />
        </button>
      )}
    </div>
  );
}
