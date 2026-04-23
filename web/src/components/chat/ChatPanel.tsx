import { useEffect, useState } from 'react';
import { useChatStore } from '../../store/chatStore';
import { useEventStore } from '../../store/eventStore';
import { chatApi } from '../../utils/api';
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
  const conversationId = getRuntimeConversationId(agentId);

  useEffect(() => { ensureSession(conversationId); }, [conversationId, ensureSession]);

  useEffect(() => {
    setLoading(true);
    chatApi.getMessages(conversationId).then((msgs) => {
      const rich = msgs
        .filter((m) => m.role === 'user' || m.role === 'assistant')
        .map(messageToRichMessage);
      loadHistory(conversationId, rich);
    }).catch(() => {}).finally(() => setLoading(false));
  }, [conversationId, loadHistory]);

  // §13 / Track-A: subscribe to chat partition (card:<conversationID>).
  // ChatReducers (registered globally in eventStore.installEnvelopeWiring)
  // pick up chat.message.sent / chat.callback.* envelopes and update
  // chatStore directly.
  useEffect(() => {
    if (!conversationId) return;
    return useEventStore.getState().trackSubscribe(`card:${conversationId}`);
  }, [conversationId]);

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
