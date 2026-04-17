import { Outlet, NavLink, useParams, useNavigate } from 'react-router-dom';
import { useEffect, useState, useCallback, useRef } from 'react';
import { useTranslation } from 'react-i18next';
import { ArrowLeft } from 'lucide-react';
import * as icons from 'lucide-react';
import { agentApi } from '../../utils/api';
import { agentDetailTabs } from '../../constants/navigation';
import { useWebSocket } from '../../hooks/useWebSocket';
import type { Agent } from '../../types/app';
import LoadingSpinner from '../common/LoadingSpinner';

export default function AgentDetailLayout() {
  const { t } = useTranslation();
  const { id } = useParams<{ id: string }>();
  const navigate = useNavigate();
  const [agent, setAgent] = useState<Agent | null>(null);
  const [loading, setLoading] = useState(true);
  const refreshTimerRef = useRef<ReturnType<typeof setTimeout>>(undefined);

  useEffect(() => {
    if (!id) return;
    setLoading(true);
    agentApi.get(id).then(setAgent).catch(() => navigate('/agents')).finally(() => setLoading(false));
  }, [id, navigate]);

  const handleWsMessage = useCallback((data: unknown) => {
    const msg = data as { type?: string; agent_id?: string };
    if (msg.type === 'graph_changed' && msg.agent_id === id) {
      clearTimeout(refreshTimerRef.current);
      refreshTimerRef.current = setTimeout(() => {
        agentApi.get(id!).then(setAgent).catch(() => {});
      }, 300);
    }
  }, [id]);

  useEffect(() => {
    return () => clearTimeout(refreshTimerRef.current);
  }, []);

  useWebSocket('/api/ws', { onMessage: handleWsMessage, reconnectInterval: 5000 });

  if (loading) return <div className="flex items-center justify-center h-full"><LoadingSpinner /></div>;

  // Don't render Outlet if agent is not loaded yet
  if (!agent) return <div className="flex items-center justify-center h-full"><LoadingSpinner /></div>;

  return (
    <div className="flex flex-col h-full">
      <div className="flex items-center gap-4 px-6 py-3 border-b border-gray-200 dark:border-gray-800 bg-white dark:bg-gray-900 shrink-0">
        <button onClick={() => navigate('/agents')} className="p-1.5 rounded-lg hover:bg-gray-100 dark:hover:bg-gray-800 text-gray-500">
          <ArrowLeft size={18} />
        </button>
        <div className="flex-1 min-w-0">
          <h1 className="text-lg font-semibold text-gray-900 dark:text-gray-100 truncate">{agent.name}</h1>
          {agent.description && <p className="text-xs text-gray-500 truncate">{agent.description}</p>}
        </div>
        <nav className="flex gap-1">
          {agentDetailTabs.map((tab) => {
            const Icon = (icons as unknown as Record<string, React.FC<{ size?: number }>>)[tab.icon] || icons.Circle;
            return (
              <NavLink
                key={tab.path}
                to={tab.path}
                className={({ isActive }) =>
                  `flex items-center gap-1.5 px-3 py-1.5 rounded-lg text-sm transition-colors ${
                    isActive ? 'bg-indigo-50 dark:bg-indigo-950 text-indigo-700 dark:text-indigo-300 font-medium' : 'text-gray-500 hover:bg-gray-100 dark:hover:bg-gray-800'
                  }`
                }
              >
                <Icon size={14} />
                <span className="hidden lg:inline">{t(tab.labelKey)}</span>
              </NavLink>
            );
          })}
        </nav>
      </div>
      <div className="flex-1 overflow-auto">
        <Outlet context={{ agent, setAgent }} />
      </div>
    </div>
  );
}
