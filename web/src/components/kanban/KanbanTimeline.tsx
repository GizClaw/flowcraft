import { useEffect, useState } from 'react';
import { kanbanApi } from '../../utils/api';
import type { TimelineEntry, CardStatus } from '../../types/kanban';
import { Clock, AlertCircle, CheckCircle2, Loader2, Timer } from 'lucide-react';

const AGENT_COLORS = [
  'bg-violet-400', 'bg-cyan-400', 'bg-amber-400', 'bg-rose-400',
  'bg-emerald-400', 'bg-fuchsia-400', 'bg-sky-400', 'bg-orange-400',
];

const AGENT_DOT_COLORS = [
  'bg-violet-500', 'bg-cyan-500', 'bg-amber-500', 'bg-rose-500',
  'bg-emerald-500', 'bg-fuchsia-500', 'bg-sky-500', 'bg-orange-500',
];

function agentColorIndex(agentId: string): number {
  let hash = 0;
  for (let i = 0; i < agentId.length; i++) {
    hash = (hash * 31 + agentId.charCodeAt(i)) | 0;
  }
  return Math.abs(hash) % AGENT_COLORS.length;
}

interface Props {
  runtimeId: string;
}

const statusConfig: Record<CardStatus, { bg: string; bar: string; icon: React.ReactNode }> = {
  pending: {
    bg: 'bg-gray-50 dark:bg-gray-800',
    bar: 'bg-gray-300 dark:bg-gray-600',
    icon: <Clock size={13} className="text-gray-400" />,
  },
  claimed: {
    bg: 'bg-blue-50 dark:bg-blue-950',
    bar: 'bg-blue-400',
    icon: <Loader2 size={13} className="text-blue-500 animate-spin" />,
  },
  done: {
    bg: 'bg-green-50 dark:bg-green-950',
    bar: 'bg-green-400',
    icon: <CheckCircle2 size={13} className="text-green-500" />,
  },
  failed: {
    bg: 'bg-red-50 dark:bg-red-950',
    bar: 'bg-red-400',
    icon: <AlertCircle size={13} className="text-red-500" />,
  },
};

function formatElapsed(ms?: number): string {
  if (!ms) return '-';
  if (ms < 1000) return `${ms}ms`;
  if (ms < 60_000) return `${(ms / 1000).toFixed(1)}s`;
  return `${(ms / 60_000).toFixed(1)}m`;
}

function formatTime(iso: string): string {
  try {
    return new Date(iso).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', second: '2-digit' });
  } catch {
    return '';
  }
}

export default function KanbanTimeline({ runtimeId }: Props) {
  const [entries, setEntries] = useState<TimelineEntry[]>([]);
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    setLoading(true);
    kanbanApi.timeline()
      .then(setEntries)
      .catch(() => {})
      .finally(() => setLoading(false));
  }, [runtimeId]);

  if (loading) {
    return <div className="flex items-center justify-center h-64 text-gray-400 text-sm">Loading...</div>;
  }

  if (entries.length === 0) {
    return <div className="flex items-center justify-center h-64 text-gray-400 text-sm">No timeline data</div>;
  }

  const minTime = Math.min(...entries.map((e) => new Date(e.created_at).getTime()));
  const maxTime = Math.max(...entries.map((e) => new Date(e.updated_at).getTime()));
  const span = maxTime - minTime || 1;
  const totalElapsed = maxTime - minTime;

  return (
    <div className="p-4 space-y-3">
      {/* Summary bar */}
      <div className="flex items-center justify-between px-2">
        <div className="flex items-center gap-4">
          <span className="text-xs text-gray-500 flex items-center gap-1">
            <Timer size={12} />
            Total: {formatElapsed(totalElapsed)}
          </span>
          <span className="text-xs text-gray-400">{entries.length} cards</span>
        </div>
        <div className="flex items-center gap-3">
          {(['done', 'failed', 'claimed', 'pending'] as const).map((s) => {
            const count = entries.filter((e) => e.status === s).length;
            if (!count) return null;
            const cfg = statusConfig[s];
            return (
              <span key={s} className="flex items-center gap-1 text-[11px] text-gray-500">
                {cfg.icon} {count}
              </span>
            );
          })}
        </div>
      </div>

      {/* Agent legend */}
      {(() => {
        const agentIds = [...new Set(entries.map(e => e.target_agent_id).filter(Boolean))] as string[];
        if (agentIds.length === 0) return null;
        return (
          <div className="flex flex-wrap items-center gap-3 px-2 mt-1">
            {agentIds.map(id => (
              <span key={id} className="flex items-center gap-1 text-[10px] text-gray-500">
                <span className={`w-2 h-2 rounded-full ${AGENT_DOT_COLORS[agentColorIndex(id)]}`} />
                {id}
              </span>
            ))}
          </div>
        );
      })()}

      {/* Gantt chart */}
      <div className="overflow-x-auto">
        <div className="min-w-[600px] relative">
          {/* Time axis */}
          <div className="flex justify-between px-1 mb-1">
            <span className="text-[10px] text-gray-400">{formatTime(entries[0]?.created_at)}</span>
            <span className="text-[10px] text-gray-400">{formatTime(entries[entries.length - 1]?.updated_at)}</span>
          </div>
          <div className="h-px bg-gray-200 dark:bg-gray-700 mb-3" />

          {/* Entries */}
          <div className="space-y-1.5">
            {entries.map((entry) => {
              const cfg = statusConfig[entry.status];
              const barColor = entry.target_agent_id
                ? AGENT_COLORS[agentColorIndex(entry.target_agent_id)]
                : cfg.bar;
              const start = new Date(entry.created_at).getTime();
              const end = new Date(entry.updated_at).getTime();
              const left = ((start - minTime) / span) * 100;
              const width = Math.max(((end - start) / span) * 100, 2);

              return (
                <div key={entry.card_id} className={`rounded-lg ${cfg.bg} p-2.5`}>
                  <div className="flex items-center gap-3">
                    {/* Info */}
                    <div className="w-48 shrink-0">
                      <div className="flex items-center gap-1.5">
                        {cfg.icon}
                        <span className="text-xs font-medium text-gray-700 dark:text-gray-300 truncate">
                          {entry.target_agent_id || entry.template || entry.type}
                        </span>
                      </div>
                      {entry.query && (
                        <p className="text-[11px] text-gray-500 dark:text-gray-400 line-clamp-1 mt-0.5 ml-5">
                          {entry.query}
                        </p>
                      )}
                    </div>

                    {/* Bar */}
                    <div className="flex-1 relative h-5 bg-gray-100 dark:bg-gray-800 rounded-full overflow-hidden">
                      <div
                        className={`absolute top-0 h-full rounded-full ${barColor} opacity-80 transition-all`}
                        style={{ left: `${left}%`, width: `${width}%` }}
                      />
                    </div>

                    {/* Meta */}
                    <div className="w-28 shrink-0 text-right space-y-0.5">
                      {(entry.target_agent_id || entry.agent_id) && (
                        <div className="flex items-center gap-1 justify-end">
                          {entry.target_agent_id && (
                            <span className={`w-2 h-2 rounded-full shrink-0 ${AGENT_DOT_COLORS[agentColorIndex(entry.target_agent_id)]}`} />
                          )}
                          <p className="text-[10px] text-gray-400 truncate">{entry.target_agent_id || entry.agent_id}</p>
                        </div>
                      )}
                      <p className="text-[10px] text-gray-500 font-medium">{formatElapsed(entry.elapsed_ms)}</p>
                    </div>
                  </div>

                  {entry.error && (
                    <div className="flex items-center gap-1 mt-1.5 ml-5 text-[11px] text-red-500">
                      <AlertCircle size={10} />
                      <span className="line-clamp-1">{entry.error}</span>
                    </div>
                  )}
                </div>
              );
            })}
          </div>
        </div>
      </div>
    </div>
  );
}
