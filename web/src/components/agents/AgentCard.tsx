import { useNavigate } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { MoreVertical, Trash2 } from 'lucide-react';
import { useState } from 'react';
import { formatDistanceToNow } from 'date-fns';
import type { Agent } from '../../types/app';
import ConfirmDialog from '../common/ConfirmDialog';
import { agentApi } from '../../utils/api';
import { useToastStore } from '../../store/toastStore';
import { COPILOT_AGENT_ID } from '../../store/copilotStore';

interface Props {
  agent: Agent;
  onDeleted?: () => void;
}

export default function AgentCard({ agent, onDeleted }: Props) {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const [showMenu, setShowMenu] = useState(false);
  const [showDelete, setShowDelete] = useState(false);
  const addToast = useToastStore((s) => s.addToast);

  const handleDelete = async () => {
    try {
      await agentApi.delete(agent.id);
      addToast('success', t('agents.deleted', { name: agent.name }));
      onDeleted?.();
    } catch (err) {
      addToast('error', err instanceof Error ? err.message : t('agents.deleteFailed'));
    }
  };

  return (
    <>
      <div
        onClick={() => navigate(`/agents/${agent.id}/editor`)}
        className="group relative bg-white dark:bg-gray-900 rounded-xl border border-gray-200 dark:border-gray-800 p-4 hover:shadow-lg hover:border-indigo-300 dark:hover:border-indigo-700 cursor-pointer transition-all"
      >
        <div className="flex items-start justify-between mb-2">
          <h3 className="text-sm font-semibold text-gray-900 dark:text-gray-100 truncate pr-2">{agent.name}</h3>
          {agent.id !== COPILOT_AGENT_ID && (
            <div className="relative">
              <button
                onClick={(e) => { e.stopPropagation(); setShowMenu(!showMenu); }}
                className="p-1 rounded hover:bg-gray-100 dark:hover:bg-gray-800 opacity-0 group-hover:opacity-100 transition-opacity"
              >
                <MoreVertical size={14} className="text-gray-400" />
              </button>
              {showMenu && (
                <>
                  <div className="fixed inset-0 z-10" onClick={(e) => { e.stopPropagation(); setShowMenu(false); }} />
                  <div className="absolute right-0 top-full mt-1 w-32 bg-white dark:bg-gray-800 border border-gray-200 dark:border-gray-700 rounded-lg shadow-lg z-20">
                    <button
                      onClick={(e) => { e.stopPropagation(); setShowMenu(false); setShowDelete(true); }}
                      className="w-full flex items-center gap-2 px-3 py-2 text-sm text-red-600 hover:bg-red-50 dark:hover:bg-red-950 rounded-lg"
                    >
                      <Trash2 size={14} /> {t('agents.delete')}
                    </button>
                  </div>
                </>
              )}
            </div>
          )}
        </div>
        {agent.description && <p className="text-xs text-gray-500 mb-3 line-clamp-2">{agent.description}</p>}
        <div className="flex items-center gap-2 text-[10px] text-gray-400">
          <span className="px-1.5 py-0.5 rounded bg-gray-100 dark:bg-gray-800">{agent.type}</span>
          <span>{formatDistanceToNow(new Date(agent.updated_at), { addSuffix: true })}</span>
        </div>
      </div>

      <ConfirmDialog
        open={showDelete}
        onClose={() => setShowDelete(false)}
        onConfirm={handleDelete}
        title={t('agents.deleteTitle')}
        message={t('agents.deleteMessage', { name: agent.name })}
        confirmLabel={t('agents.delete')}
        variant="danger"
      />
    </>
  );
}
