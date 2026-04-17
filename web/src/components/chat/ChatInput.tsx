import { useState, useRef } from 'react';
import { useTranslation } from 'react-i18next';
import { Send, Square } from 'lucide-react';

interface Props {
  onSend: (message: string) => void;
  onStop?: () => void;
  isStreaming?: boolean;
  placeholder?: string;
}

export default function ChatInput({ onSend, onStop, isStreaming, placeholder }: Props) {
  const { t } = useTranslation();
  const [input, setInput] = useState('');
  const inputRef = useRef<HTMLTextAreaElement>(null);

  const handleSubmit = () => {
    const msg = input.trim();
    if (!msg || isStreaming) return;
    onSend(msg);
    setInput('');
    inputRef.current?.focus();
  };

  return (
    <div className="flex items-end gap-2 p-4 border-t border-gray-200 dark:border-gray-800 bg-white dark:bg-gray-900">
      <textarea
        ref={inputRef}
        value={input}
        onChange={(e) => setInput(e.target.value)}
        onKeyDown={(e) => {
          if (e.key === 'Enter' && !e.shiftKey) { e.preventDefault(); handleSubmit(); }
        }}
        placeholder={placeholder || t('chat.placeholder')}
        rows={1}
        className="flex-1 px-4 py-2.5 text-sm rounded-xl border border-gray-300 dark:border-gray-600 bg-gray-50 dark:bg-gray-800 focus:ring-2 focus:ring-indigo-500 focus:border-transparent resize-none max-h-32"
      />
      {isStreaming ? (
        <button onClick={onStop} className="p-2.5 rounded-xl bg-red-500 text-white hover:bg-red-600 shrink-0">
          <Square size={18} />
        </button>
      ) : (
        <button
          onClick={handleSubmit}
          disabled={!input.trim()}
          className="p-2.5 rounded-xl bg-indigo-600 text-white hover:bg-indigo-700 disabled:opacity-50 disabled:cursor-not-allowed shrink-0"
        >
          <Send size={18} />
        </button>
      )}
    </div>
  );
}
