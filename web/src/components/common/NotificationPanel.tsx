import { useTranslation } from 'react-i18next';
import { Check, Trash2, CheckCircle, AlertCircle, Info } from 'lucide-react';
import { useNotificationStore, type Notification } from '../../store/notificationStore';
import { formatDistanceToNow } from 'date-fns';

const typeIcons: Record<Notification['type'], typeof CheckCircle> = {
  success: CheckCircle,
  error: AlertCircle,
  info: Info,
};

const typeColors: Record<Notification['type'], string> = {
  success: 'text-green-500',
  error: 'text-red-500',
  info: 'text-blue-500',
};

export default function NotificationPanel({ onClose }: { onClose: () => void }) {
  const { t } = useTranslation();
  const notifications = useNotificationStore((s) => s.notifications);
  const markRead = useNotificationStore((s) => s.markRead);
  const markAllRead = useNotificationStore((s) => s.markAllRead);
  const clear = useNotificationStore((s) => s.clear);

  return (
    <>
      <div className="fixed inset-0 z-40" onClick={onClose} />
      <div className="absolute right-0 top-full mt-1 w-80 bg-white dark:bg-gray-900 border border-gray-200 dark:border-gray-700 rounded-xl shadow-xl z-50 max-h-96 flex flex-col">
        <div className="flex items-center justify-between px-4 py-3 border-b border-gray-200 dark:border-gray-700">
          <h3 className="text-sm font-semibold text-gray-900 dark:text-gray-100">{t('notifications.title')}</h3>
          <div className="flex gap-1">
            <button onClick={markAllRead} className="p-1 rounded hover:bg-gray-100 dark:hover:bg-gray-800 text-gray-400" title={t('notifications.markAllRead')}>
              <Check size={14} />
            </button>
            <button onClick={clear} className="p-1 rounded hover:bg-gray-100 dark:hover:bg-gray-800 text-gray-400" title={t('notifications.clearAll')}>
              <Trash2 size={14} />
            </button>
          </div>
        </div>
        <div className="overflow-y-auto flex-1">
          {notifications.length === 0 ? (
            <p className="p-4 text-sm text-gray-400 text-center">{t('notifications.empty')}</p>
          ) : (
            notifications.slice(0, 50).map((n) => {
              const Icon = typeIcons[n.type];
              return (
                <div
                  key={n.id}
                  onClick={() => markRead(n.id)}
                  className={`flex items-start gap-3 px-4 py-3 border-b border-gray-100 dark:border-gray-800 cursor-pointer hover:bg-gray-50 dark:hover:bg-gray-800 ${!n.read ? 'bg-blue-50/50 dark:bg-blue-950/30' : ''}`}
                >
                  <Icon size={16} className={`mt-0.5 shrink-0 ${typeColors[n.type]}`} />
                  <div className="flex-1 min-w-0">
                    <p className="text-sm font-medium text-gray-800 dark:text-gray-200 truncate">{n.title}</p>
                    <p className="text-xs text-gray-500 truncate">{n.message}</p>
                    <p className="text-xs text-gray-400 mt-0.5">{formatDistanceToNow(new Date(n.timestamp), { addSuffix: true })}</p>
                  </div>
                  {!n.read && <div className="w-2 h-2 rounded-full bg-blue-500 mt-1.5 shrink-0" />}
                </div>
              );
            })
          )}
        </div>
      </div>
    </>
  );
}
