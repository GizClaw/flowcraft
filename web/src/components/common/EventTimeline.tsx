import { formatDistanceToNow } from 'date-fns';
import { CheckCircle, XCircle, Clock, SkipForward, Play } from 'lucide-react';
import type { ExecutionEvent } from '../../types/chat';

const iconMap: Record<string, typeof CheckCircle> = {
  node_start: Play,
  node_complete: CheckCircle,
  node_error: XCircle,
  node_skipped: SkipForward,
  graph_start: Clock,
  graph_end: CheckCircle,
};

const colorMap: Record<string, string> = {
  node_start: 'text-blue-500',
  node_complete: 'text-green-500',
  node_error: 'text-red-500',
  node_skipped: 'text-gray-400',
  graph_start: 'text-indigo-500',
  graph_end: 'text-green-600',
};

export default function EventTimeline({ events }: { events: ExecutionEvent[] }) {
  return (
    <div className="space-y-2">
      {events.map((event) => {
        const Icon = iconMap[event.type] || Clock;
        const color = colorMap[event.type] || 'text-gray-500';
        return (
          <div key={event.id} className="flex items-start gap-3 text-sm">
            <Icon size={16} className={`mt-0.5 shrink-0 ${color}`} />
            <div className="flex-1 min-w-0">
              <div className="flex items-center gap-2">
                <span className="font-medium text-gray-800 dark:text-gray-200">{event.type}</span>
                {event.node_id && <span className="text-xs px-1.5 py-0.5 rounded bg-gray-100 dark:bg-gray-800 text-gray-600 dark:text-gray-400">{event.node_id}</span>}
                {event.payload?.elapsed_ms != null && <span className="text-xs text-gray-400">{Number(event.payload.elapsed_ms)}ms</span>}
              </div>
              {event.payload?.error != null && <p className="text-xs text-red-500 mt-0.5">{String(event.payload.error)}</p>}
              <p className="text-xs text-gray-400">{formatDistanceToNow(new Date(event.created_at), { addSuffix: true })}</p>
            </div>
          </div>
        );
      })}
    </div>
  );
}
