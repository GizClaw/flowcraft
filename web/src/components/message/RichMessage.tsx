import type React from 'react';
import { User, Bot } from 'lucide-react';
import ToolCallList from './ToolCallList';
import MarkdownContent from './MarkdownContent';
import type { RichMessage as RichMsg } from '../../types/chat';

interface Props {
  message: RichMsg;
  showAvatar?: boolean;
  renderExtra?: (msg: RichMsg) => React.ReactNode;
}

export default function RichMessage({ message, showAvatar = true, renderExtra }: Props) {
  const isUser = message.role === 'user';

  const body = (
    <div className="space-y-2">
      {renderExtra?.(message)}
      {message.toolCalls && message.toolCalls.length > 0 && (
        <ToolCallList toolCalls={message.toolCalls} />
      )}
      {message.content && (
        <div className="bg-gray-100 dark:bg-gray-800 rounded-2xl rounded-bl-md px-4 py-2.5 overflow-hidden">
          <MarkdownContent content={message.content} />
        </div>
      )}
    </div>
  );

  return (
    <div className={`flex gap-3 ${isUser ? 'flex-row-reverse' : ''}`}>
      {showAvatar ? (
        <div className={`w-7 h-7 rounded-full flex items-center justify-center shrink-0 ${isUser ? 'bg-indigo-100 dark:bg-indigo-900' : 'bg-emerald-100 dark:bg-emerald-900'}`}>
          {isUser ? <User size={14} className="text-indigo-500" /> : <Bot size={14} className="text-emerald-500" />}
        </div>
      ) : (
        <div className="w-7 shrink-0" />
      )}
      <div className={`max-w-[85%] ${isUser ? 'bg-indigo-600 text-white rounded-2xl rounded-br-md px-4 py-2.5 text-sm' : ''}`}>
        {isUser ? (
          <p className="whitespace-pre-wrap break-words [overflow-wrap:anywhere] text-sm">{message.content}</p>
        ) : body}
      </div>
    </div>
  );
}
