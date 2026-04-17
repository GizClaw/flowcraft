import { memo } from 'react';
import { useTranslation } from 'react-i18next';
import { useShallow } from 'zustand/react/shallow';
import { useKanbanStore } from '../../store/kanbanStore';
import KanbanCard from './KanbanCard';
import type { CardStatus } from '../../types/kanban';
import { KANBAN_COLUMN_COLORS } from '../../constants/colors';

const COLUMN_STATUSES: CardStatus[] = ['pending', 'claimed', 'done', 'failed'];
const COLUMN_I18N_KEYS: Record<CardStatus, string> = {
  pending: 'kanban.pending',
  claimed: 'kanban.claimed',
  done: 'kanban.done',
  failed: 'kanban.failed',
};

interface KanbanColumnProps {
  status: CardStatus;
}

const KanbanColumn = memo(function KanbanColumn({ status }: KanbanColumnProps) {
  const { t } = useTranslation();
  const cards = useKanbanStore(
    useShallow((s) => s.getCardsByStatus(status))
  );

  return (
    <div className={`rounded-xl ${KANBAN_COLUMN_COLORS[status]} p-3 flex flex-col min-h-0`}>
      <div className="flex items-center justify-between mb-3 px-1">
        <h3 className="text-xs font-semibold text-gray-600 dark:text-gray-400 uppercase tracking-wide">
          {t(COLUMN_I18N_KEYS[status])}
        </h3>
        <span className="text-[10px] font-medium px-1.5 py-0.5 rounded-full bg-white/70 dark:bg-gray-700/70 text-gray-500">
          {cards.length}
        </span>
      </div>
      <div className="flex-1 space-y-2 overflow-y-auto">
        {cards.length === 0 ? (
          <p className="text-xs text-gray-400 text-center py-8">{t('kanban.noCards')}</p>
        ) : (
          cards.map((card) => <KanbanCard key={card.id} card={card} />)
        )}
      </div>
    </div>
  );
});

export default function KanbanBoard() {
  return (
    <div className="flex h-full">
      <div className="flex-1 grid grid-cols-4 gap-3 p-4 overflow-y-auto">
        {COLUMN_STATUSES.map((status) => (
          <KanbanColumn key={status} status={status} />
        ))}
      </div>
    </div>
  );
}
