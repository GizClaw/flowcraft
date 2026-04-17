import { useTranslation } from 'react-i18next';
import { CheckCircle, XCircle, Loader2, Send, ArrowRight } from 'lucide-react';
import type { DispatchedTask } from '../../types/chat';
import { labelKeyForTemplate } from '../../store/copilotStore';

export default function TaskDispatchCard({ task, onViewTasks }: { task: DispatchedTask; onViewTasks?: () => void }) {
  const { t } = useTranslation();
  const statusIcon = task.status === 'submitted' || task.status === 'running'
    ? <Loader2 size={12} className="animate-spin text-violet-500" />
    : task.status === 'success'
      ? <CheckCircle size={12} className="text-emerald-500" />
      : <XCircle size={12} className="text-red-500" />;

  const statusText = t(`copilot.status.${task.status}`);
  const borderColor = task.status === 'submitted' || task.status === 'running'
    ? 'border-violet-200 dark:border-violet-800'
    : task.status === 'success'
      ? 'border-emerald-200 dark:border-emerald-800'
      : 'border-red-200 dark:border-red-800';
  const bgColor = task.status === 'submitted' || task.status === 'running'
    ? 'bg-violet-50/50 dark:bg-violet-950/30'
    : task.status === 'success'
      ? 'bg-emerald-50/30 dark:bg-emerald-950/20'
      : 'bg-red-50/30 dark:bg-red-950/20';

  return (
    <div className={`border rounded-lg overflow-hidden ${borderColor} ${bgColor}`}>
      <div className="flex items-center gap-2 px-3 py-2">
        <Send size={12} className="text-violet-500 shrink-0" />
        <span className="text-xs font-semibold text-violet-600 dark:text-violet-400">{labelKeyForTemplate(task.template) ? t(labelKeyForTemplate(task.template)!) : task.template}</span>
        <span className="text-[11px] text-gray-500 dark:text-gray-400">{statusText}</span>
        <span className="shrink-0 ml-auto flex items-center gap-1.5">
          {statusIcon}
          {onViewTasks && (
            <button
              onClick={onViewTasks}
              className="text-[11px] text-violet-500 hover:text-violet-700 dark:hover:text-violet-300 flex items-center gap-0.5"
            >
              {t('copilot.viewDetails')} <ArrowRight size={10} />
            </button>
          )}
        </span>
      </div>
    </div>
  );
}
