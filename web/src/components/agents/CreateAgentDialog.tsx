import { useState, useEffect } from 'react';
import { useTranslation } from 'react-i18next';
import { Brain, GitBranch } from 'lucide-react';
import Modal from '../common/Modal';
import { agentApi, templateApi } from '../../utils/api';
import type { GraphTemplate, CreateAgentRequest, Agent } from '../../types/app';
import { useToastStore } from '../../store/toastStore';

interface Props {
  open: boolean;
  onClose: () => void;
  onCreated: (agent: Agent) => void;
}

const templateIcons: Record<string, React.ReactNode> = {
  blank: <GitBranch size={24} className="text-gray-400" />,
  react_agent: <Brain size={24} className="text-purple-500" />,
};

export default function CreateAgentDialog({ open, onClose, onCreated }: Props) {
  const { t } = useTranslation();
  const [templates, setTemplates] = useState<GraphTemplate[]>([]);
  const [name, setName] = useState('');
  const [description, setDescription] = useState('');
  const [selectedTemplate, setSelectedTemplate] = useState<string>('blank');
  const [creating, setCreating] = useState(false);
  const addToast = useToastStore((s) => s.addToast);

  useEffect(() => {
    if (open) {
      templateApi.list()
        .then((t) => setTemplates(t.filter((x) => !x.name.startsWith('copilot'))))
        .catch((err) => {
          console.error('Failed to load templates:', err);
          addToast('error', t('createAgent.templateFailed'));
        });
    }
  }, [open, addToast, t]);

  const handleCreate = async () => {
    if (!name.trim()) return;
    setCreating(true);
    try {
      const req: CreateAgentRequest = { name: name.trim(), type: 'workflow', description: description.trim() || undefined };
      if (selectedTemplate !== 'blank') req.template = selectedTemplate;
      const agent = await agentApi.create(req);
      onCreated(agent);
      onClose();
      setName('');
      setDescription('');
      setSelectedTemplate('blank');
    } catch (err) {
      addToast('error', err instanceof Error ? err.message : t('createAgent.createFailed'));
    } finally {
      setCreating(false);
    }
  };

  return (
    <Modal open={open} onClose={onClose} title={t('createAgent.title')} size="lg">
      <div className="space-y-4">
        <div>
          <label className="block text-sm font-medium text-gray-700 dark:text-gray-300 mb-1">{t('createAgent.name')}</label>
          <input
            value={name}
            onChange={(e) => setName(e.target.value)}
            placeholder={t('createAgent.namePlaceholder')}
            className="w-full px-3 py-2 text-sm rounded-lg border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-800"
            autoFocus
          />
        </div>
        <div>
          <label className="block text-sm font-medium text-gray-700 dark:text-gray-300 mb-1">{t('createAgent.description')}</label>
          <input
            value={description}
            onChange={(e) => setDescription(e.target.value)}
            placeholder={t('createAgent.descPlaceholder')}
            className="w-full px-3 py-2 text-sm rounded-lg border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-800"
          />
        </div>
        <div>
          <label className="block text-sm font-medium text-gray-700 dark:text-gray-300 mb-2">{t('createAgent.template')}</label>
          <div className="grid grid-cols-3 gap-2">
            {templates.map((tmpl) => (
              <TemplateCard
                key={tmpl.name}
                name={tmpl.name}
                label={tmpl.label}
                description={tmpl.description}
                icon={templateIcons[tmpl.name] || <GitBranch size={24} className="text-gray-400" />}
                selected={selectedTemplate === tmpl.name}
                onSelect={() => setSelectedTemplate(tmpl.name)}
              />
            ))}
          </div>
        </div>
        <button
          onClick={handleCreate}
          disabled={!name.trim() || creating}
          className="w-full px-4 py-2.5 bg-indigo-600 text-white rounded-lg hover:bg-indigo-700 disabled:opacity-50 text-sm font-medium"
        >
          {creating ? t('createAgent.creating') : t('createAgent.createButton')}
        </button>
      </div>
    </Modal>
  );
}

function TemplateCard({ label, description, icon, selected, onSelect }: {
  name: string; label: string; description: string; icon: React.ReactNode; selected: boolean; onSelect: () => void;
}) {
  return (
    <button
      onClick={onSelect}
      className={`flex flex-col items-center gap-2 p-3 rounded-lg border-2 text-center transition-colors ${selected ? 'border-indigo-500 bg-indigo-50 dark:bg-indigo-950' : 'border-gray-200 dark:border-gray-700 hover:border-gray-300 dark:hover:border-gray-600'}`}
    >
      {icon}
      <span className="text-xs font-medium text-gray-800 dark:text-gray-200">{label}</span>
      <span className="text-[10px] text-gray-500 line-clamp-2">{description}</span>
    </button>
  );
}
