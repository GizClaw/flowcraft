import { useOutletContext } from 'react-router-dom';
import ChatPanel from '../components/chat/ChatPanel';
import type { Agent } from '../types/app';

export default function AgentChatPage() {
  const { agent } = useOutletContext<{ agent: Agent }>();
  return (
    <div className="h-full">
      <ChatPanel agentId={agent.id} />
    </div>
  );
}
