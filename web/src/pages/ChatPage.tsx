import { useState, useEffect } from 'react';
import { agentApi } from '../utils/api';
import type { Agent } from '../types/app';
import ChatPanel from '../components/chat/ChatPanel';
import LoadingSpinner from '../components/common/LoadingSpinner';
import { COPILOT_AGENT_ID } from '../store/copilotStore';

export default function ChatPage() {
  const [agents, setAgents] = useState<Agent[]>([]);
  const [selectedAgent, setSelectedAgent] = useState<string>('');
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    setLoading(true);
    agentApi.list()
      .then((list) => {
        const filtered = list.filter((a) => a.id !== COPILOT_AGENT_ID);
        setAgents(filtered);
        if (filtered.length > 0) setSelectedAgent(filtered[0].id);
      })
      .catch(() => {})
      .finally(() => setLoading(false));
  }, []);

  if (loading) return <div className="flex justify-center py-16"><LoadingSpinner /></div>;

  return (
    <div className="flex flex-col h-full">
      <div className="px-4 py-3 border-b border-gray-200 dark:border-gray-800 bg-white dark:bg-gray-900 shrink-0">
        <select
          value={selectedAgent}
          onChange={(e) => setSelectedAgent(e.target.value)}
          className="px-3 py-1.5 text-sm rounded-lg border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-800"
        >
          {agents.map((a) => <option key={a.id} value={a.id}>{a.name}</option>)}
        </select>
      </div>
      <div className="flex-1 overflow-hidden">
        {selectedAgent && <ChatPanel agentId={selectedAgent} />}
      </div>
    </div>
  );
}
