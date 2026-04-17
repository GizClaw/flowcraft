import { useToastStore } from '../store/toastStore';
import type { Agent, CreateAgentRequest, UpdateAgentRequest, CompileResult, DryRunResult, GraphTemplate } from '../types/app';
import type { Conversation, Message, ChatRequest, WorkflowStreamEvent, WorkflowRun, ExecutionEvent, ResumeRequest } from '../types/chat';
import type { Dataset, DatasetDocument, CreateDatasetRequest, AddDocumentRequest, QueryDatasetRequest, QueryResult } from '../types/knowledge';
import type { NodeSchema } from '../types/nodeTypes';
import type { Plugin } from '../types/plugin';
import type { KanbanSnapshot, TimelineEntry, TopologyNode, TopologyEdge } from '../types/kanban';

export class ApiError extends Error {
  status: number;
  constructor(status: number, message: string) {
    super(message);
    this.name = 'ApiError';
    this.status = status;
  }
}

interface FetchOptions extends RequestInit {
  json?: unknown;
}

export async function apiFetch<T = unknown>(path: string, opts: FetchOptions = {}): Promise<T> {
  const { json, headers: extra, ...rest } = opts;
  const headers: Record<string, string> = { ...(extra as Record<string, string>) };

  if (json !== undefined) {
    headers['Content-Type'] = 'application/json';
    rest.body = JSON.stringify(json);
  }

  const res = await fetch(path, { headers, credentials: 'include', ...rest });

  if (!res.ok) {
    const body = await res.json().catch(() => ({}));
    const err = (body as { error?: { message?: string }; message?: string });
    throw new ApiError(res.status, err.error?.message || err.message || res.statusText);
  }

  const warning = res.headers.get('X-Warning');
  if (warning) {
    useToastStore.getState().addToast('warning', warning);
  }

  if (res.status === 204) return undefined as unknown as T;
  return res.json() as Promise<T>;
}

export async function* apiStream<T = unknown>(path: string, opts: FetchOptions & { signal?: AbortSignal } = {}): AsyncGenerator<T> {
  const { json, headers: extra, signal, ...rest } = opts;
  const headers: Record<string, string> = { ...(extra as Record<string, string>) };

  if (json !== undefined) {
    headers['Content-Type'] = 'application/json';
    rest.body = JSON.stringify(json);
  }

  const res = await fetch(path, { headers, signal, credentials: 'include', ...rest });

  if (!res.ok) {
    const body = await res.json().catch(() => ({}));
    const err = (body as { error?: { message?: string }; message?: string });
    throw new ApiError(res.status, err.error?.message || err.message || res.statusText);
  }

  const reader = res.body?.getReader();
  if (!reader) return;

  const decoder = new TextDecoder();
  let buffer = '';
  let currentEventType = '';

  try {
    while (true) {
      const { done, value } = await reader.read();
      if (done) break;

      buffer += decoder.decode(value, { stream: true });
      const lines = buffer.split('\n');
      buffer = lines.pop() || '';

      for (const line of lines) {
        const trimmed = line.trim();
        if (trimmed.startsWith('event: ')) {
          currentEventType = trimmed.slice(7).trim();
        } else if (trimmed.startsWith('data: ')) {
          try {
            const parsed = JSON.parse(trimmed.slice(6));
            if (currentEventType && typeof parsed === 'object' && parsed !== null) {
              (parsed as Record<string, unknown>).type = currentEventType;
            }
            yield parsed as T;
          } catch { /* skip malformed */ }
          currentEventType = '';
        } else if (trimmed === '') {
          currentEventType = '';
        }
      }
    }
  } finally {
    reader.releaseLock();
  }
}

// ── Agent API ──

export const agentApi = {
  list: () => apiFetch<{ data: Agent[]; has_more: boolean }>('/api/agents').then(r => r.data ?? []),
  get: (id: string) => apiFetch<Agent>(`/api/agents/${id}`),
  create: (data: CreateAgentRequest) => apiFetch<Agent>('/api/agents', { method: 'POST', json: data }),
  update: (id: string, data: UpdateAgentRequest) => apiFetch<Agent>(`/api/agents/${id}`, { method: 'PUT', json: data }),
  delete: (id: string) => apiFetch<void>(`/api/agents/${id}`, { method: 'DELETE' }),
  abort: (id: string) => apiFetch<{ aborted: boolean }>(`/api/agents/${id}/abort`, { method: 'POST' }),
};

export interface AuthStatus {
  initialized: boolean;
  authenticated: boolean;
  username?: string;
}

export const authApi = {
  status: () => apiFetch<AuthStatus>('/api/auth/status'),
  setup: (username: string, password: string) =>
    apiFetch<{ ok: boolean }>('/api/auth/setup', { method: 'POST', json: { username, password } }),
  login: (username: string, password: string) =>
    apiFetch<{ token: string; expires_at: string }>('/api/auth/login', { method: 'POST', json: { username, password } }),
  logout: () => apiFetch<void>('/api/auth/logout', { method: 'POST' }),
  session: () => apiFetch<AuthStatus>('/api/auth/session'),
  changePassword: (oldPassword: string, newPassword: string) =>
    apiFetch<{ ok: boolean }>('/api/auth/change-password', { method: 'POST', json: { old_password: oldPassword, new_password: newPassword } }),
};

// ── Chat API ──

export const chatApi = {
  stream: (data: ChatRequest) => apiStream<WorkflowStreamEvent>('/api/chat/stream', { method: 'POST', json: data }),
  streamReconnect: (data: { agent_id: string; conversation_id?: string }) =>
    apiStream<WorkflowStreamEvent>('/api/chat/stream', { method: 'POST', json: { ...data, query: '', reconnect: true } }),
  resumeStream: (agentId: string, data: ResumeRequest) => apiStream<WorkflowStreamEvent>('/api/chat/resume/stream', { method: 'POST', json: { ...data, agent_id: agentId } }),
  getConversations: (agentId?: string) => apiFetch<{ data: Conversation[] }>(`/api/conversations${agentId ? `?agent_id=${agentId}` : ''}`).then(r => r.data ?? []),
  getMessages: (conversationId: string) => apiFetch<{ data: Message[] }>(`/api/conversations/${conversationId}/messages`).then(r => r.data ?? []),
};

// ── Compile / DryRun ──

export const compileApi = {
  compile: (agentId: string) => apiFetch<CompileResult>(`/api/agents/${agentId}/compile`, { method: 'POST' }),
  dryrun: (agentId: string) => apiFetch<DryRunResult>(`/api/agents/${agentId}/dryrun`, { method: 'POST' }),
};

// ── Import / Export ──

export const importExportApi = {
  exportGraph: async (agentId: string, format: 'json' | 'yaml' = 'json') => {
    const res = await fetch(`/api/agents/${agentId}/export?format=${format}`, { credentials: 'include' });
    if (!res.ok) throw new Error('Export failed');
    return res.text();
  },
  importGraph: (agentId: string, data: { format: string; content: string; force?: boolean }) =>
    apiFetch<CompileResult>(`/api/agents/${agentId}/import`, { method: 'POST', json: data }),
};

// ── Version API ──

export interface GraphVersion {
  id: string;
  agent_id: string;
  version: number;
  graph_definition: Record<string, unknown> | null;
  description?: string;
  checksum: string;
  created_by?: string;
  published_at?: string | null;
  created_at: string;
}

export interface GraphDiff {
  nodes_added?: { id: string; type: string }[];
  nodes_removed?: { id: string; type: string }[];
  nodes_changed?: { node_id: string; before: Record<string, unknown>; after: Record<string, unknown> }[];
  edges_added?: { from: string; to: string }[];
  edges_removed?: { from: string; to: string }[];
}

export const versionApi = {
  list: (agentId: string) => apiFetch<{ data: GraphVersion[] }>(`/api/agents/${agentId}/versions`).then(r => r.data ?? []),
  publish: (agentId: string, version: number, description?: string) =>
    apiFetch<GraphVersion>(`/api/agents/${agentId}/versions/publish`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ version, description }),
    }),
  rollback: (agentId: string, version: number) => apiFetch<void>(`/api/agents/${agentId}/versions/${version}/rollback`, { method: 'POST' }),
  diff: (agentId: string, v1: number, v2: number) => apiFetch<GraphDiff>(`/api/agents/${agentId}/versions/diff?v1=${v1}&v2=${v2}`),
};

// ── Dataset / Knowledge API ──

export const datasetApi = {
  list: () => apiFetch<{ data: Dataset[] }>('/api/datasets').then(r => r.data ?? []),
  get: (id: string) => apiFetch<Dataset>(`/api/datasets/${id}`),
  create: (data: CreateDatasetRequest) => apiFetch<Dataset>('/api/datasets', { method: 'POST', json: data }),
  delete: (id: string) => apiFetch<void>(`/api/datasets/${id}`, { method: 'DELETE' }),
  listDocuments: (datasetId: string) => apiFetch<{ data: DatasetDocument[] }>(`/api/datasets/${datasetId}/documents`).then(r => r.data ?? []),
  addDocument: (datasetId: string, data: AddDocumentRequest) => apiFetch<DatasetDocument>(`/api/datasets/${datasetId}/documents`, { method: 'POST', json: data }),
  deleteDocument: (datasetId: string, docId: string) => apiFetch<void>(`/api/datasets/${datasetId}/documents/${docId}`, { method: 'DELETE' }),
  query: (datasetId: string, data: QueryDatasetRequest) => apiFetch<{ data: QueryResult[] }>(`/api/datasets/${datasetId}/query`, { method: 'POST', json: data }).then(r => r.data ?? []),
};

// ── Node Types API ──

export const nodeTypeApi = {
  list: () => apiFetch<{ data: NodeSchema[] }>('/api/node-types').then(r => r.data ?? []),
};

// ── Template API ──

export const templateApi = {
  list: () => apiFetch<{ data: GraphTemplate[] }>('/api/templates').then(r => r.data ?? []),
  instantiate: (name: string) => apiFetch<Agent>(`/api/templates/${name}/instantiate`, { method: 'POST' }),
};

// ── Model / Provider API ──

export interface ConfiguredModel {
  provider: string;
  model: string;
  label: string;
  is_default: boolean;
}

export interface ProviderModelOption {
  label: string;
  name: string;
}

export interface ProviderInfo {
  name: string;
  configured: boolean;
  models: ProviderModelOption[];
}

export const modelApi = {
  list: () => apiFetch<{ data: ConfiguredModel[] }>('/api/models').then(r => r.data ?? []),
  add: (data: { provider: string; model: string; api_key?: string; base_url?: string; extra?: Record<string, unknown> }) =>
    apiFetch<ConfiguredModel>('/api/models', { method: 'POST', json: data }),
  setDefault: (provider: string, model: string) => apiFetch<void>('/api/models/default', { method: 'PUT', json: { provider, model } }),
  delete: (id: string) => apiFetch<void>(`/api/models/${id}`, { method: 'DELETE' }),
  getProviders: () => apiFetch<{ data: ProviderInfo[] }>('/api/providers').then(r => r.data ?? []),
  configureProvider: (name: string, data: { api_key: string; base_url?: string }) =>
    apiFetch<void>(`/api/providers/${name}/configure`, { method: 'POST', json: data }),
};

// ── Tool API ──

export interface ToolItem {
  name: string;
  description: string;
}

export const toolApi = {
  list: () => apiFetch<{ data: ToolItem[] }>('/api/tools').then(r => r.data ?? []),
};

// ── Skill API ──

export interface SkillItem {
  name: string;
  description: string;
  tags?: string[];
  dir: string;
  builtin?: boolean;
}

export const skillApi = {
  list: () => apiFetch<{ data: SkillItem[] }>('/api/skills').then(r => r.data ?? []),
  install: (data: { url: string; name?: string }) => apiFetch<{ name: string }>('/api/skills/install', { method: 'POST', json: data }),
  uninstall: (name: string) => apiFetch<void>(`/api/skills/${name}`, { method: 'DELETE' }),
};

// ── Stats API ──

export interface StatsOverview {
  total_agents: number;
  total_conversations: number;
  total_runs: number;
}

export interface RunStats {
  date: string;
  count: number;
  avg_elapsed_ms: number;
}

export interface RuntimeStats {
  runtime_id: string;
  actor_count: number;
  kanban_card_count: number;
  sandbox_leases: number;
}

export interface RuntimeStatsOverview {
  runtime_id?: string;
  runtime_count: number;
  actor_count: number;
  current?: RuntimeStats;
}

export interface MemoryCategoryStats {
  category: string;
  count: number;
}

export interface MemoryStatsOverview {
  runtime_id: string;
  total_entries: number;
  categories: MemoryCategoryStats[];
}

export type MonitoringHealthStatus = 'healthy' | 'degraded' | 'down';

export interface MonitoringSummary {
  window_start: string;
  window_end: string;
  run_total: number;
  run_success: number;
  run_failed: number;
  success_rate?: number;
  error_rate?: number;
  latency_p50_ms?: number;
  latency_p95_ms?: number;
  latency_p99_ms?: number;
  health: MonitoringHealthStatus;
  health_reason?: string;
  active_actors: number;
  active_sandboxes: number;
  thresholds: {
    error_rate_warn: number;
    error_rate_down: number;
    latency_p95_warn_ms: number;
    consecutive_buckets: number;
    no_success_down_minutes: number;
  };
}

export interface MonitoringTimeseriesPoint {
  bucket_start: string;
  run_total: number;
  run_success: number;
  run_failed: number;
  success_rate?: number;
  error_rate?: number;
  latency_p50_ms?: number;
  latency_p95_ms?: number;
  latency_p99_ms?: number;
  avg_elapsed_ms?: number;
  throughput_rpm: number;
}

export interface MonitoringRuntimeOverview {
  runtime_count: number;
  actor_count: number;
  current?: {
    runtime_id: string;
    actor_count: number;
    kanban_card_count: number;
    sandbox_leases: number;
  };
}

export interface MonitoringTopFailedAgent {
  agent_id: string;
  failed_runs: number;
  total_runs: number;
  failure_rate?: number;
}

export interface MonitoringTopErrorCode {
  code: string;
  count: number;
}

export interface MonitoringRecentFailure {
  run_id: string;
  agent_id: string;
  error_code: string;
  message: string;
  elapsed_ms: number;
  created_at: string;
}

export interface MonitoringDiagnostics {
  top_failed_agents: MonitoringTopFailedAgent[];
  top_error_codes: MonitoringTopErrorCode[];
  recent_failures: MonitoringRecentFailure[];
}

export const statsApi = {
  overview: () => apiFetch<StatsOverview>('/api/stats'),
  runs: (agentId?: string) => apiFetch<{ data: RunStats[] }>(`/api/stats/runs${agentId ? `?agent_id=${agentId}` : ''}`).then(r => r.data ?? []),
  runtime: () => apiFetch<RuntimeStatsOverview>('/api/stats/runtime'),
  memory: () => apiFetch<MemoryStatsOverview>('/api/stats/memory'),
};

export const monitoringApi = {
  summary: (params?: { window?: string; agentId?: string }) => {
    const search = new URLSearchParams();
    if (params?.window) search.set('window', params.window);
    if (params?.agentId) search.set('agent_id', params.agentId);
    const query = search.toString();
    return apiFetch<MonitoringSummary>(`/api/monitoring/summary${query ? `?${query}` : ''}`);
  },
  timeseries: (params?: { window?: string; interval?: string; agentId?: string }) => {
    const search = new URLSearchParams();
    if (params?.window) search.set('window', params.window);
    if (params?.interval) search.set('interval', params.interval);
    if (params?.agentId) search.set('agent_id', params.agentId);
    const query = search.toString();
    return apiFetch<{ data: MonitoringTimeseriesPoint[] }>(`/api/monitoring/timeseries${query ? `?${query}` : ''}`).then(r => r.data ?? []);
  },
  runtime: () => apiFetch<MonitoringRuntimeOverview>('/api/monitoring/runtime'),
  diagnostics: (params?: { window?: string; agentId?: string; limit?: number }) => {
    const search = new URLSearchParams();
    if (params?.window) search.set('window', params.window);
    if (params?.agentId) search.set('agent_id', params.agentId);
    if (params?.limit) search.set('limit', String(params.limit));
    const query = search.toString();
    return apiFetch<MonitoringDiagnostics>(`/api/monitoring/diagnostics${query ? `?${query}` : ''}`);
  },
};

// ── Workflow Run API ──

export const workflowRunApi = {
  list: (agentId?: string) => apiFetch<{ data: WorkflowRun[] }>(`/api/workflows/runs${agentId ? `?agent_id=${agentId}` : ''}`).then(r => r.data ?? []),
  get: (id: string) => apiFetch<WorkflowRun>(`/api/workflows/runs/${id}`),
  status: (id: string) => apiFetch<{ status: string }>(`/api/workflows/runs/${id}/status`),
  events: (id: string) => apiFetch<{ data: ExecutionEvent[] }>(`/api/workflows/runs/${id}/events`).then(r => r.data ?? []),
};

// ── Kanban API ──

export const kanbanApi = {
  cards: () => apiFetch<{ data: KanbanSnapshot['cards'] }>('/api/kanban/cards').then(r => r.data ?? []),
  timeline: () => apiFetch<{ data: TimelineEntry[] }>('/api/kanban/timeline').then(r => r.data ?? []),
  topology: () => apiFetch<{ nodes: TopologyNode[]; edges: TopologyEdge[] }>('/api/kanban/topology'),
};

// ── Plugin API ──

export const pluginApi = {
  list: () => apiFetch<{ data: Plugin[] }>('/api/plugins').then(r => r.data ?? []),
  get: (name: string) => apiFetch<Plugin>(`/api/plugins/${name}`),
  enable: (name: string) => apiFetch<Plugin>(`/api/plugins/${name}/enable`, { method: 'POST' }),
  disable: (name: string) => apiFetch<Plugin>(`/api/plugins/${name}/disable`, { method: 'POST' }),
  configure: (name: string, config: Record<string, unknown>) =>
    apiFetch<Plugin>(`/api/plugins/${name}/config`, { method: 'PUT', json: config }),
  reload: () =>
    apiFetch<{ added: number; removed: number }>('/api/plugins/reload', { method: 'POST' }),
  upload: (file: File) => {
    const form = new FormData();
    form.append('file', file);
    return apiFetch<{ id: string; name: string; size: number; added: number; removed: number }>(
      '/api/plugins/upload', { method: 'POST', body: form },
    );
  },
  remove: (name: string) => apiFetch<void>(`/api/plugins/${name}`, { method: 'DELETE' }),
};

// ── Channel Types API ──

export interface ChannelTypeField {
  type: string;
  required?: boolean;
  secret?: boolean;
}

export interface ChannelTypeSchema {
  type: string;
  label: string;
  config_schema: Record<string, ChannelTypeField>;
}

export const channelApi = {
  types: () => apiFetch<{ data: ChannelTypeSchema[] }>('/api/channel-types').then(r => r.data ?? []),
};

export interface WSTicket {
  ticket: string;
  expires_at: string;
}

export const wsApi = {
  ticket: () => apiFetch<WSTicket>('/api/ws-ticket', { method: 'POST' }),
};

// ── Long-term Memory API ──

export interface MemoryEntry {
  id: string;
  category: string;
  content: string;
  source?: {
    runtime_id?: string;
    conversation_id?: string;
  };
  created_at: string;
  updated_at: string;
}

export const memoryApi = {
  list: (category?: string) => {
    const p = new URLSearchParams();
    if (category) p.set('category', category);
    const qs = p.toString();
    return apiFetch<{ data: MemoryEntry[] }>(`/api/memories${qs ? `?${qs}` : ''}`).then(r => r.data ?? []);
  },
  update: (entryId: string, content: string) =>
    apiFetch<MemoryEntry>(`/api/memories/${entryId}`, { method: 'PUT', json: { content } }),
  delete: (entryId: string) =>
    apiFetch<void>(`/api/memories/${entryId}`, { method: 'DELETE' }),
};

// ── Setup API ──

export const setupApi = {
  getStatus: () => modelApi.getProviders().then(providers => ({
    status: providers.some(p => p.configured) ? 'configured' as const : 'not_configured' as const,
  })),
};
