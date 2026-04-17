import { useState, useEffect } from 'react';
import { LineChart, Line, XAxis, YAxis, Tooltip, ResponsiveContainer, CartesianGrid, ComposedChart, Area, BarChart, Bar } from 'recharts';
import { AlertTriangle, CheckCircle2, ServerCrash } from 'lucide-react';
import { useTranslation } from 'react-i18next';
import { useNavigate } from 'react-router-dom';
import { monitoringApi, agentApi, type MonitoringSummary, type MonitoringTimeseriesPoint, type MonitoringRuntimeOverview, type MonitoringDiagnostics } from '../utils/api';
import LoadingSpinner from '../components/common/LoadingSpinner';

type BlockErrors = {
  summary?: string;
  timeseries?: string;
  runtime?: string;
  diagnostics?: string;
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

export default function MonitoringPage() {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const [timeWindow, setTimeWindow] = useState<'1h' | '6h' | '24h' | '7d'>('24h');
  const [interval, setInterval] = useState<'1m' | '5m' | '15m' | '1h'>('5m');
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
    const timer = globalThis.setInterval(() => setCoreRefreshTick((v) => v + 1), 30_000);
    return () => globalThis.clearInterval(timer);
  }, []);

  useEffect(() => {
    const timer = globalThis.setInterval(() => setFullRefreshTick((v) => v + 1), 120_000);
    return () => globalThis.clearInterval(timer);
  }, []);

  useEffect(() => {
    let cancelled = false;
    setLoading(true);
    Promise.allSettled([
      monitoringApi.summary({ window: timeWindow, agentId: agentId || undefined }),
      monitoringApi.timeseries({ window: timeWindow, interval, agentId: agentId || undefined }),
      monitoringApi.runtime(),
      monitoringApi.diagnostics({ window: timeWindow, agentId: agentId || undefined, limit: 8 }),
    ])
      .then((results) => {
        if (cancelled) return;
        const nextErrors: BlockErrors = {};

        const [summaryResult, timeseriesResult, runtimeResult, diagnosticsResult] = results;
        if (summaryResult.status === 'fulfilled') setSummary(summaryResult.value);
        else {
          setSummary(null);
          nextErrors.summary = t('monitoringV2.summaryLoadFailed');
        }

        if (timeseriesResult.status === 'fulfilled') setTimeseries(timeseriesResult.value);
        else {
          setTimeseries([]);
          nextErrors.timeseries = t('monitoringV2.timeseriesLoadFailed');
        }

        if (runtimeResult.status === 'fulfilled') setRuntime(runtimeResult.value);
        else {
          setRuntime(null);
          nextErrors.runtime = t('monitoringV2.runtimeLoadFailed');
        }

        if (diagnosticsResult.status === 'fulfilled') setDiagnostics(diagnosticsResult.value);
        else {
          setDiagnostics(null);
          nextErrors.diagnostics = t('monitoringV2.diagnosticsLoadFailed');
        }

        setErrors(nextErrors);
      })
      .finally(() => {
        if (!cancelled) setLoading(false);
      });
    return () => { cancelled = true; };
  }, [timeWindow, interval, agentId, reloadTick, fullRefreshTick, t]);

  useEffect(() => {
    let cancelled = false;
    Promise.allSettled([
      monitoringApi.summary({ window: timeWindow, agentId: agentId || undefined }),
      monitoringApi.runtime(),
    ]).then(([s, r]) => {
      if (cancelled) return;
      setErrors((prev) => {
        const next = { ...prev };
        if (s.status === 'fulfilled') {
          setSummary(s.value);
          delete next.summary;
        } else {
          setSummary(null);
          next.summary = t('monitoringV2.summaryLoadFailed');
        }
        if (r.status === 'fulfilled') {
          setRuntime(r.value);
          delete next.runtime;
        } else {
          setRuntime(null);
          next.runtime = t('monitoringV2.runtimeLoadFailed');
        }
        return next;
      });
    });
    return () => { cancelled = true; };
  }, [timeWindow, agentId, coreRefreshTick, t]);

  if (loading) return <div className="flex justify-center py-16"><LoadingSpinner /></div>;

  const globalError = !summary && !timeseries.length;
  if (globalError) {
    return (
      <div className="p-6">
        <div className="max-w-2xl mx-auto mt-20 bg-white dark:bg-gray-900 rounded-xl border border-red-200 dark:border-red-900 p-6 text-center">
          <h2 className="text-lg font-semibold text-red-600 dark:text-red-400">{t('monitoringV2.globalErrorTitle')}</h2>
          <p className="mt-2 text-sm text-gray-500">{t('monitoringV2.globalErrorDesc')}</p>
          <button
            onClick={() => setReloadTick((x) => x + 1)}
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
        return { icon: <ServerCrash size={18} className="text-red-500" />, label: t('monitoringV2.healthDown'), color: 'text-red-600 dark:text-red-400' };
      case 'degraded':
        return { icon: <AlertTriangle size={18} className="text-amber-500" />, label: t('monitoringV2.healthDegraded'), color: 'text-amber-600 dark:text-amber-400' };
      default:
        return { icon: <CheckCircle2 size={18} className="text-emerald-500" />, label: t('monitoringV2.healthHealthy'), color: 'text-emerald-600 dark:text-emerald-400' };
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
  const emptyState = (summary?.run_total ?? 0) === 0 && lineData.length === 0;

  return (
    <div className="p-6 space-y-6">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <h1 className="text-2xl font-bold text-gray-900 dark:text-gray-100">{t('monitoringV2.title')}</h1>
        <div className="flex items-center gap-2">
          <select value={timeWindow} onChange={(e) => setTimeWindow(e.target.value as '1h' | '6h' | '24h' | '7d')} className="px-3 py-2 rounded-lg border border-gray-200 dark:border-gray-700 bg-white dark:bg-gray-900 text-sm">
            <option value="1h">{t('monitoringV2.window.1h')}</option>
            <option value="6h">{t('monitoringV2.window.6h')}</option>
            <option value="24h">{t('monitoringV2.window.24h')}</option>
            <option value="7d">{t('monitoringV2.window.7d')}</option>
          </select>
          <select value={interval} onChange={(e) => setInterval(e.target.value as '1m' | '5m' | '15m' | '1h')} className="px-3 py-2 rounded-lg border border-gray-200 dark:border-gray-700 bg-white dark:bg-gray-900 text-sm">
            <option value="1m">{t('monitoringV2.interval.1m')}</option>
            <option value="5m">{t('monitoringV2.interval.5m')}</option>
            <option value="15m">{t('monitoringV2.interval.15m')}</option>
            <option value="1h">{t('monitoringV2.interval.1h')}</option>
          </select>
          <select value={agentId} onChange={(e) => setAgentId(e.target.value)} className="px-3 py-2 rounded-lg border border-gray-200 dark:border-gray-700 bg-white dark:bg-gray-900 text-sm">
            <option value="">{t('monitoringV2.allAgents')}</option>
            {agents.map((a) => <option key={a.id} value={a.id}>{a.name}</option>)}
          </select>
        </div>
      </div>

      {!!Object.keys(errors).length && (
        <div className="bg-amber-50 dark:bg-amber-950/30 border border-amber-200 dark:border-amber-900 rounded-xl p-3 text-sm text-amber-700 dark:text-amber-300">
          {t('monitoringV2.partialErrorPrefix')}{Object.values(errors).join('；')}
        </div>
      )}

      {emptyState && (
        <div className="bg-white dark:bg-gray-900 rounded-xl border border-gray-200 dark:border-gray-800 p-8 text-center">
          <p className="text-gray-500">{t('monitoringV2.emptyDesc')}</p>
        </div>
      )}

      <div className="grid grid-cols-2 lg:grid-cols-4 gap-4">
        <div className="bg-white dark:bg-gray-900 rounded-xl border border-gray-200 dark:border-gray-800 p-4">
          <div className="text-sm text-gray-500 mb-1">{t('monitoringV2.healthStatus')}</div>
          <div className={`flex items-center gap-2 text-lg font-semibold ${healthMeta.color}`}>
            {healthMeta.icon}
            {healthMeta.label}
          </div>
          {summary?.health_reason && <div className="text-xs text-gray-500 mt-2">{summary.health_reason}</div>}
        </div>
        <div className="bg-white dark:bg-gray-900 rounded-xl border border-gray-200 dark:border-gray-800 p-4">
          <div className="text-sm text-gray-500 mb-1">{t('monitoringV2.successRate')}</div>
          <div className="text-2xl font-bold text-gray-900 dark:text-gray-100">{errors.summary ? '--' : (summary?.success_rate != null ? `${(summary.success_rate * 100).toFixed(2)}%` : '--')}</div>
          <div className="text-xs text-gray-500 mt-2">{t('monitoringV2.errorRate')} {errors.summary ? '--' : (summary?.error_rate != null ? `${(summary.error_rate * 100).toFixed(2)}%` : '--')}</div>
        </div>
        <div className="bg-white dark:bg-gray-900 rounded-xl border border-gray-200 dark:border-gray-800 p-4">
          <div className="text-sm text-gray-500 mb-1">{t('monitoringV2.p95Latency')}</div>
          <div className="text-2xl font-bold text-gray-900 dark:text-gray-100">{errors.summary ? '--' : (summary?.latency_p95_ms ? `${summary.latency_p95_ms.toFixed(0)} ms` : '--')}</div>
          <div className="text-xs text-gray-500 mt-2">{t('monitoringV2.p99Latency')} {summary?.latency_p99_ms ? `${summary.latency_p99_ms.toFixed(0)} ms` : '-'}</div>
        </div>
        <div className="bg-white dark:bg-gray-900 rounded-xl border border-gray-200 dark:border-gray-800 p-4">
          <div className="text-sm text-gray-500 mb-1">{t('monitoringV2.runLoad')}</div>
          <div className="text-2xl font-bold text-gray-900 dark:text-gray-100">{errors.summary ? '--' : (summary?.run_total ?? '--')}</div>
          <div className="text-xs text-gray-500 mt-2">{t('monitoringV2.actors')} {errors.runtime ? '--' : (runtime?.current?.actor_count ?? '--')} / {t('monitoringV2.sandboxes')} {errors.runtime ? '--' : (runtime?.current?.sandbox_leases ?? '--')}</div>
        </div>
      </div>

      <div className="grid grid-cols-1 xl:grid-cols-2 gap-4">
        <div className="bg-white dark:bg-gray-900 rounded-xl border border-gray-200 dark:border-gray-800 p-4">
          <div className="flex items-center gap-2 mb-4">
            <h3 className="text-sm font-semibold text-gray-700 dark:text-gray-300">{t('monitoringV2.qualityTitle')}</h3>
            {qualityAlert && <span className="text-xs px-2 py-0.5 rounded bg-amber-100 text-amber-700 dark:bg-amber-900/40 dark:text-amber-300">{t('monitoringV2.qualityAlert')}</span>}
          </div>
          <ResponsiveContainer width="100%" height={280}>
            <LineChart data={lineData}>
              <CartesianGrid strokeDasharray="3 3" stroke="#374151" />
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
            <h3 className="text-sm font-semibold text-gray-700 dark:text-gray-300">{t('monitoringV2.capacityTitle')}</h3>
            {capacityAlert && <span className="text-xs px-2 py-0.5 rounded bg-red-100 text-red-700 dark:bg-red-900/40 dark:text-red-300">{t('monitoringV2.capacityAlert')}</span>}
          </div>
          <ResponsiveContainer width="100%" height={280}>
            <ComposedChart data={lineData}>
              <CartesianGrid strokeDasharray="3 3" stroke="#374151" />
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
          <h3 className="text-sm font-semibold text-gray-700 dark:text-gray-300 mb-4">{t('monitoringV2.topFailedAgents')}</h3>
          <ResponsiveContainer width="100%" height={280}>
            <BarChart data={diagnostics?.top_failed_agents ?? []}>
              <CartesianGrid strokeDasharray="3 3" stroke="#374151" />
              <XAxis dataKey="agent_id" tick={{ fontSize: 12 }} />
              <YAxis tick={{ fontSize: 12 }} />
              <Tooltip />
              <Bar dataKey="failed_runs" fill="#ef4444" radius={[4, 4, 0, 0]} />
            </BarChart>
          </ResponsiveContainer>
        </div>

        <div className="bg-white dark:bg-gray-900 rounded-xl border border-gray-200 dark:border-gray-800 p-4">
          <h3 className="text-sm font-semibold text-gray-700 dark:text-gray-300 mb-4">{t('monitoringV2.recentFailures')}</h3>
          <div className="overflow-x-auto">
            <table className="min-w-full text-sm">
              <thead>
                <tr className="text-left text-gray-500 border-b border-gray-200 dark:border-gray-800">
                  <th className="py-2 pr-3">{t('monitoringV2.tableRun')}</th>
                  <th className="py-2 pr-3">{t('monitoringV2.tableAgent')}</th>
                  <th className="py-2 pr-3">{t('monitoringV2.tableCode')}</th>
                  <th className="py-2 pr-3">{t('monitoringV2.tableElapsed')}</th>
                </tr>
              </thead>
              <tbody>
                {(diagnostics?.recent_failures ?? []).map((r) => (
                  <tr key={r.run_id} className="border-b border-gray-100 dark:border-gray-900 cursor-pointer hover:bg-gray-50 dark:hover:bg-gray-800/40" onClick={() => navigate(`/agents/${r.agent_id}/runs/${r.run_id}`)}>
                    <td className="py-2 pr-3 text-gray-700 dark:text-gray-200">{r.run_id.slice(0, 8)}</td>
                    <td className="py-2 pr-3 text-gray-700 dark:text-gray-200">{r.agent_id.slice(0, 8)}</td>
                    <td className="py-2 pr-3 text-red-500">{r.error_code}</td>
                    <td className="py-2 pr-3 text-gray-700 dark:text-gray-200">{r.elapsed_ms} ms</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </div>
      </div>
    </div>
  );
}
