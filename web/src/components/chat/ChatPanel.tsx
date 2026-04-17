import { useEffect, useState } from 'react';
import { useChatStore } from '../../store/chatStore';
import { chatApi } from '../../utils/api';
import { useWebSocket } from '../../hooks/useWebSocket';
import { handleCallbackMessage } from '../../hooks/useKanbanBoard';
import { getRuntimeConversationId } from '../../utils/runtime';
import RichChatView from './RichChatView';
import type { Message, RichMessage } from '../../types/chat';

function messageToRichMessage(msg: Message): RichMessage {
  return {
    id: msg.id,
    role: msg.role as 'user' | 'assistant',
    content: msg.content,
    timestamp: msg.created_at,
  };
}

interface Props {
  agentId: string;
}

export default function ChatPanel({ agentId }: Props) {
  const ensureSession = useChatStore((s) => s.ensureSession);
  const loadHistory = useChatStore((s) => s.loadHistory);
  const [loading, setLoading] = useState(false);

  useEffect(() => { ensureSession(agentId); }, [agentId, ensureSession]);

  useEffect(() => {
    const conversationId = getRuntimeConversationId(agentId);
    setLoading(true);
    chatApi.getMessages(conversationId).then((msgs) => {
      const rich = msgs
        .filter((m) => m.role === 'user' || m.role === 'assistant')
        .map(messageToRichMessage);
      loadHistory(agentId, rich);
    }).catch(() => {}).finally(() => setLoading(false));
  }, [agentId, loadHistory]);

  const wsUrl = '/api/ws';
  useWebSocket(wsUrl, {
    onMessage: (data) => {
      const msg = data as Record<string, unknown>;
      if ((msg.type as string)?.startsWith('callback_')) {
        handleCallbackMessage(msg, agentId);
      }
    },
    reconnectInterval: 5000,
  });

  return (
    <div className="flex h-full">
      <div className="flex-1 flex flex-col min-w-0">
        {loading ? (
          <div className="flex-1 flex justify-center items-center">
            <div className="w-6 h-6 border-2 border-indigo-600 border-t-transparent rounded-full animate-spin" />
          </div>
        ) : (
          <RichChatView agentId={agentId} />
        )}
      </div>
    </div>
  );
}
