import { useEffect, useState } from 'react';
import { statsApi, type RuntimeStatsOverview, type MemoryStatsOverview } from '../../utils/api';
import { OWNER_RUNTIME_ID } from '../../utils/runtime';

interface Props {
  runtimeId?: string;
}

export default function RuntimeDebugPanel({ runtimeId = OWNER_RUNTIME_ID }: Props) {
  const [runtimeStats, setRuntimeStats] = useState<RuntimeStatsOverview | null>(null);
  const [memoryStats, setMemoryStats] = useState<MemoryStatsOverview | null>(null);
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    let cancelled = false;
    setLoading(true);
    Promise.all([statsApi.runtime(), statsApi.memory()])
      .then(([runtime, memory]) => {
        if (cancelled) return;
        setRuntimeStats(runtime);
        setMemoryStats(memory);
      })
      .catch(() => {})
      .finally(() => {
        if (!cancelled) setLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [runtimeId]);

  const current = runtimeStats?.current;

  return (
    <div className="px-4 py-3 border-b border-gray-200 dark:border-gray-800 bg-gray-50/80 dark:bg-gray-950/50">
      <div className="flex flex-wrap items-center gap-2 text-xs">
        <span className="font-medium text-gray-700 dark:text-gray-300">Runtime Debug</span>
        <span className="px-2 py-1 rounded bg-white dark:bg-gray-900 border border-gray-200 dark:border-gray-800 text-gray-600 dark:text-gray-400">
          runtime: {runtimeId}
        </span>
        <span className="px-2 py-1 rounded bg-white dark:bg-gray-900 border border-gray-200 dark:border-gray-800 text-gray-600 dark:text-gray-400">
          runtimes: {runtimeStats?.runtime_count ?? 0}
        </span>
        <span className="px-2 py-1 rounded bg-white dark:bg-gray-900 border border-gray-200 dark:border-gray-800 text-gray-600 dark:text-gray-400">
          actors: {runtimeStats?.actor_count ?? 0}
        </span>
        <span className="px-2 py-1 rounded bg-white dark:bg-gray-900 border border-gray-200 dark:border-gray-800 text-gray-600 dark:text-gray-400">
          kanban cards: {current?.kanban_card_count ?? 0}
        </span>
        <span className="px-2 py-1 rounded bg-white dark:bg-gray-900 border border-gray-200 dark:border-gray-800 text-gray-600 dark:text-gray-400">
          sandbox leases: {current?.sandbox_leases ?? 0}
        </span>
        <span className="px-2 py-1 rounded bg-white dark:bg-gray-900 border border-gray-200 dark:border-gray-800 text-gray-600 dark:text-gray-400">
          memories: {memoryStats?.total_entries ?? 0}
        </span>
        {loading && <span className="text-gray-400">loading...</span>}
      </div>
    </div>
  );
}
