import { useState, useEffect, useMemo, useCallback, useRef } from 'react';
import { LineChart, Line, XAxis, YAxis, Tooltip, ResponsiveContainer, CartesianGrid, ComposedChart, Area, BarChart, Bar } from 'recharts';
import { AlertTriangle, CheckCircle2, ServerCrash, RefreshCw } from 'lucide-react';
import { useTranslation } from 'react-i18next';
import { useNavigate } from 'react-router-dom';
import { monitoringApi, agentApi, type MonitoringSummary, type MonitoringTimeseriesPoint, type MonitoringRuntimeOverview, type MonitoringDiagnostics } from '../utils/api';
import { useUIStore } from '../store/uiStore';
import LoadingSpinner from '../components/common/LoadingSpinner';

type BlockErrors = {
  summary?: string;
  timeseries?: string;
  runtime?: string;
  diagnostics?: string;
};

type TimeWindow = '1h' | '6h' | '24h' | '7d';
type BucketInterval = '1m' | '5m' | '15m' | '1h';

// Allowed (window, interval) pairs. Restricting these in the UI prevents
// unbounded bucket counts (e.g. 7d × 1m = 10080 points) from overwhelming
// the chart. Backend still validates independently.
const INTERVAL_OPTIONS_BY_WINDOW: Record<TimeWindow, BucketInterval[]> = {
  '1h': ['1m', '5m'],
  '6h': ['5m', '15m'],
  '24h': ['5m', '15m', '1h'],
  '7d': ['15m', '1h'],
};

const DEFAULT_INTERVAL_BY_WINDOW: Record<TimeWindow, BucketInterval> = {
  '1h': '1m',
  '6h': '5m',
  '24h': '15m',
  '7d': '1h',
};

function consecutiveBreach(points: MonitoringTimeseriesPoint[], n: number, pred: (p: MonitoringTimeseriesPoint) => boolean): boolean {
  if (points.length === 0 || n <= 0) return false;
  let streak = 0;
  for (let i = 0; i < points.length; i++) {
    if (pred(points[i])) {
      streak += 1;
      if (streak >= n) return true;
      continue;
    }
    streak = 0;
  }
  return false;
}

function shortAgentLabel(id: string, agentNames: Map<string, string>): string {
  const name = agentNames.get(id);
  if (name && name !== id) return name.length > 16 ? `${name.slice(0, 14)}…` : name;
  return id.length > 8 ? `${id.slice(0, 8)}…` : id;
}

export default function MonitoringPage() {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const isDark = useUIStore((s) => s.isDark());
  const gridStroke = isDark ? '#374151' : '#e5e7eb';

  const [timeWindow, setTimeWindow] = useState<TimeWindow>('24h');
  const [bucketInterval, setBucketInterval] = useState<BucketInterval>(DEFAULT_INTERVAL_BY_WINDOW['24h']);
  const [agentId, setAgentId] = useState('');
  const [agents, setAgents] = useState<Array<{ id: string; name: string }>>([]);
  const [summary, setSummary] = useState<MonitoringSummary | null>(null);
  const [timeseries, setTimeseries] = useState<MonitoringTimeseriesPoint[]>([]);
  const [runtime, setRuntime] = useState<MonitoringRuntimeOverview | null>(null);
  const [diagnostics, setDiagnostics] = useState<MonitoringDiagnostics | null>(null);
  const [errors, setErrors] = useState<BlockErrors>({});
  const [reloadTick, setReloadTick] = useState(0);
  const [fullRefreshTick, setFullRefreshTick] = useState(0);
  const [coreRefreshTick, setCoreRefreshTick] = useState(0);
  const [loading, setLoading] = useState(true);
  const [refreshing, setRefreshing] = useState(false);

  // Capture latest t() in a ref so we can read it inside effects without
  // adding it to the dependency array (i18n's t identity changes on every
  // language switch and would otherwise re-trigger network calls).
  const tRef = useRef(t);
  useEffect(() => { tRef.current = t; }, [t]);

  const agentNameMap = useMemo(() => {
    const m = new Map<string, string>();
    for (const a of agents) m.set(a.id, a.name);
    return m;
  }, [agents]);

  const intervalOptions = INTERVAL_OPTIONS_BY_WINDOW[timeWindow];

  useEffect(() => {
    if (!intervalOptions.includes(bucketInterval)) {
      setBucketInterval(DEFAULT_INTERVAL_BY_WINDOW[timeWindow]);
    }
  }, [timeWindow, intervalOptions, bucketInterval]);

  useEffect(() => {
    let cancelled = false;
    agentApi.list()
      .then((items) => {
        if (cancelled) return;
        setAgents(items.map((a) => ({ id: a.id, name: a.name || a.id })));
      })
      .catch(() => {
        if (!cancelled) setAgents([]);
      });
    return () => { cancelled = true; };
  }, []);

  useEffect(() => {
    const timer = window.setInterval(() => setCoreRefreshTick((v) => v + 1), 30_000);
    return () => window.clearInterval(timer);
  }, []);

  useEffect(() => {
    const timer = window.setInterval(() => setFullRefreshTick((v) => v + 1), 120_000);
    return () => window.clearInterval(timer);
  }, []);

  useEffect(() => {
    let cancelled = false;
    setLoading(true);
    setRefreshing(true);
    Promise.allSettled([
      monitoringApi.summary({ window: timeWindow, agentId: agentId || undefined }),
      monitoringApi.timeseries({ window: timeWindow, interval: bucketInterval, agentId: agentId || undefined }),
      monitoringApi.runtime(),
      monitoringApi.diagnostics({ window: timeWindow, agentId: agentId || undefined, limit: 8 }),
    ])
      .then((results) => {
        if (cancelled) return;
        const nextErrors: BlockErrors = {};
        const tt = tRef.current;

        const [summaryResult, timeseriesResult, runtimeResult, diagnosticsResult] = results;
        if (summaryResult.status === 'fulfilled') setSummary(summaryResult.value);
        else {
          setSummary(null);
          nextErrors.summary = tt('monitoring.summaryLoadFailed');
        }

        if (timeseriesResult.status === 'fulfilled') setTimeseries(timeseriesResult.value);
        else {
          setTimeseries([]);
          nextErrors.timeseries = tt('monitoring.timeseriesLoadFailed');
        }

        if (runtimeResult.status === 'fulfilled') setRuntime(runtimeResult.value);
        else {
          setRuntime(null);
          nextErrors.runtime = tt('monitoring.runtimeLoadFailed');
        }

        if (diagnosticsResult.status === 'fulfilled') setDiagnostics(diagnosticsResult.value);
        else {
          setDiagnostics(null);
          nextErrors.diagnostics = tt('monitoring.diagnosticsLoadFailed');
        }

        setErrors(nextErrors);
      })
      .finally(() => {
        if (!cancelled) {
          setLoading(false);
          setRefreshing(false);
        }
      });
    return () => { cancelled = true; };
  }, [timeWindow, bucketInterval, agentId, reloadTick, fullRefreshTick]);

  useEffect(() => {
    if (coreRefreshTick === 0) return;
    let cancelled = false;
    Promise.allSettled([
      monitoringApi.summary({ window: timeWindow, agentId: agentId || undefined }),
      monitoringApi.runtime(),
    ]).then(([s, r]) => {
      if (cancelled) return;
      const tt = tRef.current;
      setErrors((prev) => {
        const next = { ...prev };
        if (s.status === 'fulfilled') {
          setSummary(s.value);
          delete next.summary;
        } else {
          setSummary(null);
          next.summary = tt('monitoring.summaryLoadFailed');
        }
        if (r.status === 'fulfilled') {
          setRuntime(r.value);
          delete next.runtime;
        } else {
          setRuntime(null);
          next.runtime = tt('monitoring.runtimeLoadFailed');
        }
        return next;
      });
    });
    return () => { cancelled = true; };
  }, [timeWindow, agentId, coreRefreshTick]);

  const handleManualRefresh = useCallback(() => {
    setReloadTick((x) => x + 1);
  }, []);

  if (loading) return <div className="flex justify-center py-16"><LoadingSpinner /></div>;

  // Treat as a global error only when both the summary block and the
  // timeseries block failed to load. Empty data with successful responses
  // is a normal "no traffic" state and should still render the dashboard.
  const globalError = !!errors.summary && !!errors.timeseries;
  if (globalError) {
    return (
      <div className="p-6">
        <div className="max-w-2xl mx-auto mt-20 bg-white dark:bg-gray-900 rounded-xl border border-red-200 dark:border-red-900 p-6 text-center">
          <h2 className="text-lg font-semibold text-red-600 dark:text-red-400">{t('monitoring.globalErrorTitle')}</h2>
          <p className="mt-2 text-sm text-gray-500">{t('monitoring.globalErrorDesc')}</p>
          <button
            onClick={handleManualRefresh}
            className="mt-4 px-4 py-2 rounded-lg bg-indigo-600 text-white hover:bg-indigo-700"
          >
            {t('common.retry')}
          </button>
        </div>
      </div>
    );
  }

  const healthMeta = (() => {
    switch (summary?.health) {
      case 'down':
        return { icon: <ServerCrash size={18} className="text-red-500" />, label: t('monitoring.healthDown'), color: 'text-red-600 dark:text-red-400' };
      case 'degraded':
        return { icon: <AlertTriangle size={18} className="text-amber-500" />, label: t('monitoring.healthDegraded'), color: 'text-amber-600 dark:text-amber-400' };
      default:
        return { icon: <CheckCircle2 size={18} className="text-emerald-500" />, label: t('monitoring.healthHealthy'), color: 'text-emerald-600 dark:text-emerald-400' };
    }
  })();

  const thresholds = summary?.thresholds;
  const warnRate = thresholds?.error_rate_warn ?? 0.05;
  const p95Warn = thresholds?.latency_p95_warn_ms ?? 3000;
  const consecutive = thresholds?.consecutive_buckets ?? 3;
  const qualityAlert = consecutiveBreach(timeseries, consecutive, (p) => (p.error_rate ?? 0) >= warnRate);
  const capacityAlert = consecutiveBreach(timeseries, consecutive, (p) => (p.latency_p95_ms ?? 0) >= p95Warn);

  const lineData = timeseries.map((p) => ({
    time: new Date(p.bucket_start).toLocaleString([], { month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit' }),
    successRate: Math.round((p.success_rate ?? 0) * 10000) / 100,
    errorRate: Math.round((p.error_rate ?? 0) * 10000) / 100,
    p95: Math.round(p.latency_p95_ms ?? 0),
    throughput: Math.round((p.throughput_rpm ?? 0) * 100) / 100,
  }));

  const topFailedData = (diagnostics?.top_failed_agents ?? []).map((a) => ({
    ...a,
    label: shortAgentLabel(a.agent_id, agentNameMap),
  }));

  const emptyState = (summary?.run_total ?? 0) === 0 && lineData.length === 0;

  return (
    <div className="p-6 space-y-6">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <h1 className="text-2xl font-bold text-gray-900 dark:text-gray-100">{t('monitoring.title')}</h1>
        <div className="flex items-center gap-2">
          <select value={timeWindow} onChange={(e) => setTimeWindow(e.target.value as TimeWindow)} className="px-3 py-2 rounded-lg border border-gray-200 dark:border-gray-700 bg-white dark:bg-gray-900 text-sm">
            <option value="1h">{t('monitoring.window.1h')}</option>
            <option value="6h">{t('monitoring.window.6h')}</option>
            <option value="24h">{t('monitoring.window.24h')}</option>
            <option value="7d">{t('monitoring.window.7d')}</option>
          </select>
          <select value={bucketInterval} onChange={(e) => setBucketInterval(e.target.value as BucketInterval)} className="px-3 py-2 rounded-lg border border-gray-200 dark:border-gray-700 bg-white dark:bg-gray-900 text-sm">
            {intervalOptions.map((iv) => (
              <option key={iv} value={iv}>{t(`monitoring.interval.${iv}`)}</option>
            ))}
          </select>
          <select value={agentId} onChange={(e) => setAgentId(e.target.value)} className="px-3 py-2 rounded-lg border border-gray-200 dark:border-gray-700 bg-white dark:bg-gray-900 text-sm">
            <option value="">{t('monitoring.allAgents')}</option>
            {agents.map((a) => <option key={a.id} value={a.id}>{a.name}</option>)}
          </select>
          <button
            type="button"
            onClick={handleManualRefresh}
            disabled={refreshing}
            title={refreshing ? t('monitoring.refreshing') : t('monitoring.refresh')}
            aria-label={t('monitoring.refresh')}
            className="inline-flex items-center justify-center w-9 h-9 rounded-lg border border-gray-200 dark:border-gray-700 bg-white dark:bg-gray-900 text-gray-600 dark:text-gray-300 hover:bg-gray-50 dark:hover:bg-gray-800 disabled:opacity-50"
          >
            <RefreshCw size={16} className={refreshing ? 'animate-spin' : ''} />
          </button>
        </div>
      </div>

      {!!Object.keys(errors).length && (
        <div className="bg-amber-50 dark:bg-amber-950/30 border border-amber-200 dark:border-amber-900 rounded-xl p-3 text-sm text-amber-700 dark:text-amber-300">
          {t('monitoring.partialErrorPrefix')}{Object.values(errors).join('；')}
        </div>
      )}

      {emptyState && (
        <div className="bg-white dark:bg-gray-900 rounded-xl border border-gray-200 dark:border-gray-800 p-8 text-center">
          <p className="text-gray-500">{t('monitoring.emptyDesc')}</p>
        </div>
      )}

      <div className="grid grid-cols-2 lg:grid-cols-4 gap-4">
        <div className="bg-white dark:bg-gray-900 rounded-xl border border-gray-200 dark:border-gray-800 p-4">
          <div className="text-sm text-gray-500 mb-1">{t('monitoring.healthStatus')}</div>
          <div className={`flex items-center gap-2 text-lg font-semibold ${healthMeta.color}`}>
            {healthMeta.icon}
            {healthMeta.label}
          </div>
          {summary?.health_reason && <div className="text-xs text-gray-500 mt-2">{summary.health_reason}</div>}
        </div>
        <div className="bg-white dark:bg-gray-900 rounded-xl border border-gray-200 dark:border-gray-800 p-4">
          <div className="text-sm text-gray-500 mb-1">{t('monitoring.successRate')}</div>
          <div className="text-2xl font-bold text-gray-900 dark:text-gray-100">{errors.summary ? '--' : (summary?.success_rate != null ? `${(summary.success_rate * 100).toFixed(2)}%` : '--')}</div>
          <div className="text-xs text-gray-500 mt-2">{t('monitoring.errorRate')} {errors.summary ? '--' : (summary?.error_rate != null ? `${(summary.error_rate * 100).toFixed(2)}%` : '--')}</div>
        </div>
        <div className="bg-white dark:bg-gray-900 rounded-xl border border-gray-200 dark:border-gray-800 p-4">
          <div className="text-sm text-gray-500 mb-1">{t('monitoring.p95Latency')}</div>
          <div className="text-2xl font-bold text-gray-900 dark:text-gray-100">{errors.summary ? '--' : (summary?.latency_p95_ms ? `${summary.latency_p95_ms.toFixed(0)} ms` : '--')}</div>
          <div className="text-xs text-gray-500 mt-2">{t('monitoring.p99Latency')} {summary?.latency_p99_ms ? `${summary.latency_p99_ms.toFixed(0)} ms` : '-'}</div>
        </div>
        <div className="bg-white dark:bg-gray-900 rounded-xl border border-gray-200 dark:border-gray-800 p-4">
          <div className="text-sm text-gray-500 mb-1">{t('monitoring.runLoad')}</div>
          <div className="text-2xl font-bold text-gray-900 dark:text-gray-100">{errors.summary ? '--' : (summary?.run_total ?? '--')}</div>
          <div className="text-xs text-gray-500 mt-2">{t('monitoring.actors')} {errors.runtime ? '--' : (runtime?.current?.actor_count ?? '--')} / {t('monitoring.sandboxes')} {errors.runtime ? '--' : (runtime?.current?.sandbox_leases ?? '--')}</div>
        </div>
      </div>

      <div className="grid grid-cols-1 xl:grid-cols-2 gap-4">
        <div className="bg-white dark:bg-gray-900 rounded-xl border border-gray-200 dark:border-gray-800 p-4">
          <div className="flex items-center gap-2 mb-4">
            <h3 className="text-sm font-semibold text-gray-700 dark:text-gray-300">{t('monitoring.qualityTitle')}</h3>
            {qualityAlert && <span className="text-xs px-2 py-0.5 rounded bg-amber-100 text-amber-700 dark:bg-amber-900/40 dark:text-amber-300">{t('monitoring.qualityAlert')}</span>}
          </div>
          <ResponsiveContainer width="100%" height={280}>
            <LineChart data={lineData}>
              <CartesianGrid strokeDasharray="3 3" stroke={gridStroke} />
              <XAxis dataKey="time" tick={{ fontSize: 12 }} />
              <YAxis tick={{ fontSize: 12 }} />
              <Tooltip />
              <Line type="monotone" dataKey="successRate" stroke="#10b981" strokeWidth={2} dot={false} />
              <Line type="monotone" dataKey="errorRate" stroke="#ef4444" strokeWidth={2} dot={false} />
            </LineChart>
          </ResponsiveContainer>
        </div>

        <div className="bg-white dark:bg-gray-900 rounded-xl border border-gray-200 dark:border-gray-800 p-4">
          <div className="flex items-center gap-2 mb-4">
            <h3 className="text-sm font-semibold text-gray-700 dark:text-gray-300">{t('monitoring.capacityTitle')}</h3>
            {capacityAlert && <span className="text-xs px-2 py-0.5 rounded bg-red-100 text-red-700 dark:bg-red-900/40 dark:text-red-300">{t('monitoring.capacityAlert')}</span>}
          </div>
          <ResponsiveContainer width="100%" height={280}>
            <ComposedChart data={lineData}>
              <CartesianGrid strokeDasharray="3 3" stroke={gridStroke} />
              <XAxis dataKey="time" tick={{ fontSize: 12 }} />
              <YAxis tick={{ fontSize: 12 }} />
              <Tooltip />
              <Area type="monotone" dataKey="throughput" stroke="#6366f1" fill="#6366f133" />
              <Line type="monotone" dataKey="p95" stroke="#f97316" strokeWidth={2} dot={false} />
            </ComposedChart>
          </ResponsiveContainer>
        </div>
      </div>

      <div className="grid grid-cols-1 xl:grid-cols-2 gap-4">
        <div className="bg-white dark:bg-gray-900 rounded-xl border border-gray-200 dark:border-gray-800 p-4">
          <h3 className="text-sm font-semibold text-gray-700 dark:text-gray-300 mb-4">{t('monitoring.topFailedAgents')}</h3>
          <ResponsiveContainer width="100%" height={280}>
            <BarChart data={topFailedData}>
              <CartesianGrid strokeDasharray="3 3" stroke={gridStroke} />
              <XAxis dataKey="label" tick={{ fontSize: 12 }} interval={0} angle={-20} textAnchor="end" height={50} />
              <YAxis tick={{ fontSize: 12 }} allowDecimals={false} />
              <Tooltip
                labelFormatter={(_, payload) => {
                  const item = payload?.[0]?.payload as { agent_id?: string; label?: string } | undefined;
                  if (!item) return '';
                  const name = agentNameMap.get(item.agent_id ?? '');
                  return name ?? item.agent_id ?? item.label ?? '';
                }}
              />
              <Bar dataKey="failed_runs" fill="#ef4444" radius={[4, 4, 0, 0]} />
            </BarChart>
          </ResponsiveContainer>
        </div>

        <div className="bg-white dark:bg-gray-900 rounded-xl border border-gray-200 dark:border-gray-800 p-4">
          <h3 className="text-sm font-semibold text-gray-700 dark:text-gray-300 mb-4">{t('monitoring.recentFailures')}</h3>
          <div className="overflow-x-auto">
            <table className="min-w-full text-sm">
              <thead>
                <tr className="text-left text-gray-500 border-b border-gray-200 dark:border-gray-800">
                  <th className="py-2 pr-3">{t('monitoring.tableTime')}</th>
                  <th className="py-2 pr-3">{t('monitoring.tableRun')}</th>
                  <th className="py-2 pr-3">{t('monitoring.tableAgent')}</th>
                  <th className="py-2 pr-3">{t('monitoring.tableCode')}</th>
                  <th className="py-2 pr-3">{t('monitoring.tableMessage')}</th>
                  <th className="py-2 pr-3 whitespace-nowrap">{t('monitoring.tableElapsed')}</th>
                </tr>
              </thead>
              <tbody>
                {(diagnostics?.recent_failures ?? []).map((r) => {
                  const agentName = agentNameMap.get(r.agent_id);
                  const created = r.created_at ? new Date(r.created_at) : null;
                  return (
                    <tr key={r.run_id} className="border-b border-gray-100 dark:border-gray-900 cursor-pointer hover:bg-gray-50 dark:hover:bg-gray-800/40" onClick={() => navigate(`/agents/${r.agent_id}/runs/${r.run_id}`)}>
                      <td className="py-2 pr-3 text-gray-500 dark:text-gray-400 whitespace-nowrap" title={created ? created.toISOString() : ''}>
                        {created ? created.toLocaleString([], { month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit' }) : '--'}
                      </td>
                      <td className="py-2 pr-3 text-gray-700 dark:text-gray-200 font-mono" title={r.run_id}>{r.run_id.slice(0, 8)}</td>
                      <td className="py-2 pr-3 text-gray-700 dark:text-gray-200" title={r.agent_id}>{agentName ?? r.agent_id.slice(0, 8)}</td>
                      <td className="py-2 pr-3 text-red-500 whitespace-nowrap">{r.error_code}</td>
                      <td className="py-2 pr-3 text-gray-600 dark:text-gray-300 max-w-[280px] truncate" title={r.message}>{r.message || '--'}</td>
                      <td className="py-2 pr-3 text-gray-700 dark:text-gray-200 whitespace-nowrap">{r.elapsed_ms} ms</td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </div>
        </div>
      </div>
    </div>
  );
}
