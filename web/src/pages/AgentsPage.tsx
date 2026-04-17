import { useState, useEffect } from 'react';
import { useTranslation } from 'react-i18next';
import { Plus } from 'lucide-react';
import { agentApi } from '../utils/api';
import type { Agent } from '../types/app';
import AgentCard from '../components/agents/AgentCard';
import CreateAgentDialog from '../components/agents/CreateAgentDialog';
import EmptyState from '../components/common/EmptyState';
import LoadingSpinner from '../components/common/LoadingSpinner';
import { COPILOT_AGENT_ID } from '../store/copilotStore';

export default function AgentsPage() {
  const { t } = useTranslation();
  const [agents, setAgents] = useState<Agent[]>([]);
  const [loading, setLoading] = useState(true);
  const [showCreate, setShowCreate] = useState(false);

  const loadAgents = () => {
    setLoading(true);
    agentApi.list()
      .then((list) => setAgents(list.filter((a) => a.id !== COPILOT_AGENT_ID)))
      .catch(() => {})
      .finally(() => setLoading(false));
  };

  useEffect(loadAgents, []);

  return (
    <div className="p-6">
      <div className="flex items-center justify-between mb-6">
        <h1 className="text-2xl font-bold text-gray-900 dark:text-gray-100">{t('agents.title')}</h1>
        <button onClick={() => setShowCreate(true)} className="flex items-center gap-2 px-4 py-2 bg-indigo-600 text-white rounded-lg hover:bg-indigo-700 text-sm font-medium">
          <Plus size={16} /> {t('agents.createAgent')}
        </button>
      </div>

      {loading ? (
        <div className="flex justify-center py-16"><LoadingSpinner size={32} /></div>
      ) : agents.length === 0 ? (
        <EmptyState
          title={t('agents.noAgents')}
          description={t('agents.noAgentsDesc')}
          action={
            <button onClick={() => setShowCreate(true)} className="flex items-center gap-2 px-4 py-2 bg-indigo-600 text-white rounded-lg hover:bg-indigo-700 text-sm">
              <Plus size={16} /> {t('agents.createAgent')}
            </button>
          }
        />
      ) : (
        <div className="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-3 xl:grid-cols-4 gap-4">
          {agents.map((agent) => <AgentCard key={agent.id} agent={agent} onDeleted={loadAgents} />)}
        </div>
      )}

      <CreateAgentDialog
        open={showCreate}
        onClose={() => setShowCreate(false)}
        onCreated={() => { setShowCreate(false); loadAgents(); }}
      />
    </div>
  );
}
