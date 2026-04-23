import type React from 'react';
import { useRef, useEffect, useCallback, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Sparkles } from 'lucide-react';
import { Bot } from 'lucide-react';
import { useChatStore } from '../../store/chatStore';
import { useChat } from '../../hooks/useChat';
import RichMessage from '../message/RichMessage';
import ToolCallList from '../message/ToolCallList';
import MarkdownContent from '../message/MarkdownContent';
import ChatInput from './ChatInput';
import ApprovalPanel from '../workflow/ApprovalPanel';
import type { RichMessage as RichMsg } from '../../types/chat';
import { getRuntimeConversationId } from '../../utils/runtime';

export interface RichChatViewProps {
  agentId: string;
  renderExtra?: (msg: RichMsg) => React.ReactNode;
  // onBeforeSend lets callers attach extra `inputs` to the chat command
  // (formerly the buildRequest hook on useChat).
  onBeforeSend?: (content: string) => { inputs?: Record<string, unknown> };
  placeholder?: string;
  emptyIcon?: React.ReactNode;
  emptyTitle?: string;
  emptyHint?: string;
  inputComponent?: React.ReactNode;
}

export default function RichChatView({
  agentId,
  renderExtra,
  onBeforeSend,
  placeholder,
  emptyIcon,
  emptyTitle,
  emptyHint,
  inputComponent,
}: RichChatViewProps) {
  const { t } = useTranslation();
  // chatStore is keyed by conversationID (chatReducers project envelopes
  // under that key); RichChatView reads from the same key so the user
  // bubble and the streaming assistant bubble both surface here.
  const conversationId = getRuntimeConversationId(agentId);
  const session = useChatStore((s) => s.getSession(conversationId));
  const st = useChatStore((s) => s.getStreaming(conversationId));
  const isMyStream = st.isStreaming;
  const streamingContent = st.content;
  const streamingToolCalls = st.toolCalls;
  const ensureSession = useChatStore((s) => s.ensureSession);
  const scrollRef = useRef<HTMLDivElement>(null);
  const [isNearBottom, setIsNearBottom] = useState(true);
  const prevMessageCountRef = useRef(0);

  useEffect(() => { ensureSession(conversationId); }, [conversationId, ensureSession]);

  const buildRequest = useCallback((content: string) => {
    return onBeforeSend?.(content) || {};
  }, [onBeforeSend]);

  const { sendMessage: rawSend, stopStreaming, approval, submitApproval } = useChat({
    agentId,
    buildRequest,
  });

  const sendMessage = useCallback((content: string) => {
    setIsNearBottom(true);
    rawSend(content);
  }, [rawSend]);

  const handleScroll = useCallback(() => {
    const el = scrollRef.current;
    if (!el) return;
    const threshold = 80;
    setIsNearBottom(el.scrollHeight - el.scrollTop - el.clientHeight < threshold);
  }, []);

  useEffect(() => {
    const userSentNew = session.messages.length > prevMessageCountRef.current
      && session.messages.length > 0
      && session.messages[session.messages.length - 1].role === 'user';
    prevMessageCountRef.current = session.messages.length;

    if (!isNearBottom && !userSentNew) return;
    requestAnimationFrame(() => {
      scrollRef.current?.scrollTo({ top: scrollRef.current.scrollHeight, behavior: 'smooth' });
    });
  }, [session.messages, streamingContent, streamingToolCalls, isNearBottom]);

  const { messages } = session;
  const lastRole = messages.length > 0 ? messages[messages.length - 1].role : null;
  const streamContinuesAssistant = lastRole === 'assistant';

  const botAvatar = (
    <div className="w-7 h-7 rounded-full bg-emerald-100 dark:bg-emerald-900 flex items-center justify-center shrink-0">
      <Bot size={14} className="text-emerald-500" />
    </div>
  );
  const avatarPlaceholder = <div className="w-7 shrink-0" />;

  return (
    <>
      <div ref={scrollRef} onScroll={handleScroll} className="flex-1 overflow-y-auto p-4">
        {messages.length === 0 && !isMyStream && (
          <div className="flex flex-col items-center justify-center h-full text-center">
            {emptyIcon || <Sparkles size={32} className="text-indigo-300 mb-3" />}
            <p className="text-sm text-gray-500 dark:text-gray-400">{emptyTitle || t('chat.startConversation')}</p>
            {emptyHint && <p className="text-xs text-gray-400 mt-1">{emptyHint}</p>}
          </div>
        )}
        <div className="space-y-1.5">
          {messages.map((msg, idx) => {
            const prev = messages[idx - 1];
            const isFirstInGroup = !prev || prev.role !== msg.role;
            return (
              <div key={msg.id} className={isFirstInGroup && idx > 0 ? 'pt-3' : ''}>
                <RichMessage message={msg} showAvatar={isFirstInGroup} renderExtra={renderExtra} />
              </div>
            );
          })}
          {isMyStream && (streamingToolCalls.length > 0 || streamingContent) && (
            <div className={`flex gap-3 ${streamContinuesAssistant ? '' : 'pt-3'}`}>
              {streamContinuesAssistant ? avatarPlaceholder : botAvatar}
              <div className="flex-1 space-y-2 max-w-[85%]">
                {streamingToolCalls.length > 0 && <ToolCallList toolCalls={streamingToolCalls} />}
                {streamingContent && (
                  <div className="bg-gray-100 dark:bg-gray-800 rounded-2xl rounded-bl-md px-4 py-2.5 overflow-hidden">
                    <MarkdownContent content={streamingContent} streaming />
                  </div>
                )}
              </div>
            </div>
          )}
          {isMyStream && streamingToolCalls.length === 0 && !streamingContent && (
            <div className={`flex gap-3 ${streamContinuesAssistant ? '' : 'pt-3'}`}>
              {streamContinuesAssistant ? avatarPlaceholder : botAvatar}
              <div className="flex items-center gap-1.5 px-4 py-2.5">
                <div className="w-1.5 h-1.5 rounded-full bg-gray-400 animate-bounce [animation-delay:0ms]" />
                <div className="w-1.5 h-1.5 rounded-full bg-gray-400 animate-bounce [animation-delay:150ms]" />
                <div className="w-1.5 h-1.5 rounded-full bg-gray-400 animate-bounce [animation-delay:300ms]" />
              </div>
            </div>
          )}
        </div>
      </div>
      {approval.pending && (
        <div className="px-4 pb-2">
          <ApprovalPanel
            prompt={approval.prompt}
            onDecision={submitApproval}
            disabled={isMyStream}
          />
        </div>
      )}
      {inputComponent || (
        <ChatInput onSend={sendMessage} onStop={stopStreaming} isStreaming={isMyStream} placeholder={placeholder} />
      )}
    </>
  );
}
