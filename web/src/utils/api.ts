import client, { ApiError, apiStream } from '../api/client';
import type { components } from '../api/schema';
import type { Agent, CreateAgentRequest, UpdateAgentRequest, CompileResult, DryRunResult, GraphTemplate } from '../types/app';
import type { Conversation, Message, ChatRequest, WorkflowStreamEvent, WorkflowRun, ExecutionEvent, ResumeRequest } from '../types/chat';
import type { Dataset, DatasetDocument, CreateDatasetRequest, AddDocumentRequest, QueryDatasetRequest, QueryResult } from '../types/knowledge';
import type { NodeSchema } from '../types/nodeTypes';
import type { KanbanSnapshot, TimelineEntry, TopologyNode, TopologyEdge } from '../types/kanban';
import type { Plugin } from '../types/plugin';

export { ApiError } from '../api/client';
export { apiStream } from '../api/client';

export type Schemas = components['schemas'];

// ── Agent API ──

export const agentApi = {
  list: async () => {
    const { data } = await client.GET('/agents');
    return (data?.data ?? []) as Agent[];
  },
  get: async (id: string) => {
    const { data } = await client.GET('/agents/{id}', { params: { path: { id } } });
    return data as unknown as Agent;
  },
  create: async (body: CreateAgentRequest) => {
    const { data } = await client.POST('/agents', {
      body: body as unknown as Schemas['CreateAgentRequest'],
    });
    return data as unknown as Agent;
  },
  update: async (id: string, body: UpdateAgentRequest) => {
    const { data } = await client.PUT('/agents/{id}', {
      params: { path: { id } },
      body: body as unknown as Schemas['UpdateAgentRequest'],
    });
    return data as unknown as Agent;
  },
  delete: async (id: string) => {
    await client.DELETE('/agents/{id}', { params: { path: { id } } });
  },
  abort: async (id: string) => {
    const { data } = await client.POST('/agents/{agentID}/abort', {
      params: { path: { agentID: id } },
    });
    return data as { aborted?: boolean };
  },
};

// ── Auth API ──

export interface AuthStatus {
  initialized?: boolean;
  authenticated?: boolean;
  auth_enabled?: boolean;
  username?: string;
  principal?: string;
  auth_mode?: string;
}

async function authFetch<T>(path: string, opts?: RequestInit & { json?: unknown }): Promise<T> {
  const { json, headers: extra, ...rest } = opts ?? {};
  const headers: Record<string, string> = { ...(extra as Record<string, string>) };
  if (json !== undefined) {
    headers['Content-Type'] = 'application/json';
    rest.body = JSON.stringify(json);
  }
  const res = await fetch(path, { headers, credentials: 'include', ...rest });
  if (!res.ok) {
    const body = await res.json().catch(() => ({}));
    const err = body as { error?: { message?: string }; message?: string };
    throw new ApiError(res.status, err.error?.message || err.message || res.statusText);
  }
  if (res.status === 204) return undefined as unknown as T;
  return res.json() as Promise<T>;
}

export const authApi = {
  status: () => authFetch<AuthStatus>('/api/auth/status'),
  setup: (username: string, password: string) =>
    authFetch<{ ok: boolean }>('/api/auth/setup', { method: 'POST', json: { username, password } }),
  login: (username: string, password: string) =>
    authFetch<{ token: string; expires_at: string }>('/api/auth/login', { method: 'POST', json: { username, password } }),
  logout: () => authFetch<void>('/api/auth/logout', { method: 'POST' }),
  session: () => authFetch<AuthStatus>('/api/auth/session'),
  changePassword: (oldPassword: string, newPassword: string) =>
    authFetch<{ ok: boolean }>('/api/auth/change-password', { method: 'POST', json: { old_password: oldPassword, new_password: newPassword } }),
};

// ── Chat API ──

export const chatApi = {
  stream: (data: ChatRequest) =>
    apiStream<WorkflowStreamEvent>('/api/chat/stream', {
      method: 'POST',
      json: data,
    }),
  streamReconnect: (data: { agent_id: string; conversation_id?: string }) =>
    apiStream<WorkflowStreamEvent>('/api/chat/stream', {
      method: 'POST',
      json: { ...data, query: '', reconnect: true },
    }),
  resumeStream: (agentId: string, data: ResumeRequest) =>
    apiStream<WorkflowStreamEvent>('/api/chat/resume/stream', {
      method: 'POST',
      json: { ...data, agent_id: agentId },
    }),
  getConversations: async (agentId?: string) => {
    const { data } = await client.GET('/conversations', {
      params: { query: agentId ? { agent_id: agentId } : {} },
    });
    return (data?.data ?? []) as Conversation[];
  },
  getMessages: async (conversationId: string) => {
    const { data } = await client.GET('/conversations/{id}/messages', {
      params: { path: { id: conversationId } },
    });
    return (data?.data ?? []) as Message[];
  },
};

// ── Compile / DryRun ──

export const compileApi = {
  compile: async (agentId: string) => {
    const { data } = await client.POST('/agents/{id}/compile', {
      params: { path: { id: agentId } },
    });
    return data as unknown as CompileResult;
  },
  dryrun: async (agentId: string) => {
    const { data } = await client.POST('/agents/{id}/dryrun', {
      params: { path: { id: agentId } },
    });
    return data as unknown as DryRunResult;
  },
};

// ── Import / Export ──

export const importExportApi = {
  exportGraph: async (agentId: string, format: 'json' | 'yaml' = 'json') => {
    const res = await fetch(`/api/agents/${agentId}/export?format=${format}`, {
      credentials: 'include',
    });
    if (!res.ok) throw new Error('Export failed');
    return res.text();
  },
  importGraph: async (agentId: string, data: { format: string; content: string; force?: boolean }) => {
    const { data: result } = await client.POST('/agents/{id}/import', {
      params: { path: { id: agentId } },
      body: data as Schemas['ImportRequest'],
    });
    return result as unknown as CompileResult;
  },
};

// ── Version API ──

export interface GraphVersion {
  id: string;
  agent_id: string;
  version: number;
  graph_definition?: Record<string, unknown> | null;
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
  list: async (agentId: string) => {
    const { data } = await client.GET('/agents/{id}/versions', {
      params: { path: { id: agentId } },
    });
    return (data?.data ?? []) as GraphVersion[];
  },
  publish: async (agentId: string, _version: number, description?: string) => {
    const { data } = await client.POST('/agents/{id}/versions/publish', {
      params: { path: { id: agentId } },
      body: { description },
    });
    return data as GraphVersion;
  },
  rollback: async (agentId: string, version: number) => {
    const { data } = await client.POST('/agents/{id}/versions/{ver}/rollback', {
      params: { path: { id: agentId, ver: version } },
    });
    return data as GraphVersion;
  },
  diff: async (agentId: string, v1: number, v2: number) => {
    const { data } = await client.GET('/agents/{id}/versions/diff', {
      params: { path: { id: agentId }, query: { v1, v2 } },
    });
    return data as GraphDiff;
  },
};

// ── Dataset / Knowledge API ──

export const datasetApi = {
  list: async () => {
    const { data } = await client.GET('/datasets');
    return (data?.data ?? []) as Dataset[];
  },
  get: async (id: string) => {
    const { data } = await client.GET('/datasets/{id}', {
      params: { path: { id } },
    });
    return data as unknown as Dataset;
  },
  create: async (body: CreateDatasetRequest) => {
    const { data } = await client.POST('/datasets', {
      body: body as Schemas['CreateDatasetRequest'],
    });
    return data as unknown as Dataset;
  },
  delete: async (id: string) => {
    await client.DELETE('/datasets/{id}', { params: { path: { id } } });
  },
  listDocuments: async (datasetId: string) => {
    const { data } = await client.GET('/datasets/{id}/documents', {
      params: { path: { id: datasetId } },
    });
    return (data?.data ?? []) as DatasetDocument[];
  },
  addDocument: async (datasetId: string, body: AddDocumentRequest) => {
    const { data } = await client.POST('/datasets/{id}/documents', {
      params: { path: { id: datasetId } },
      body: body as Schemas['AddDocumentRequest'],
    });
    return data as unknown as DatasetDocument;
  },
  deleteDocument: async (datasetId: string, docId: string) => {
    await client.DELETE('/datasets/{id}/documents/{docId}', {
      params: { path: { id: datasetId, docId } },
    });
  },
  query: async (datasetId: string, body: QueryDatasetRequest) => {
    const { data } = await client.POST('/datasets/{id}/query', {
      params: { path: { id: datasetId } },
      body: body as Schemas['DatasetQueryRequest'],
    });
    return (data?.data ?? []) as QueryResult[];
  },
};

// ── Node Types API ──

export const nodeTypeApi = {
  list: async () => {
    const { data } = await client.GET('/node-types');
    return (data?.data ?? []) as NodeSchema[];
  },
};

// ── Template API ──

export const templateApi = {
  list: async () => {
    const { data } = await client.GET('/templates');
    return (data?.data ?? []) as unknown as GraphTemplate[];
  },
  instantiate: async (name: string) => {
    const { data } = await client.POST('/templates/{name}/instantiate', {
      params: { path: { name } },
    });
    return data as unknown as Agent;
  },
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
  list: async () => {
    const { data } = await client.GET('/models');
    return (data?.data ?? []) as ConfiguredModel[];
  },
  add: async (body: { provider: string; model: string; api_key?: string; base_url?: string; extra?: Record<string, unknown> }) => {
    const { data } = await client.POST('/models', {
      body: body as Schemas['AddModelRequest'],
    });
    return data as unknown as ConfiguredModel;
  },
  setDefault: async (provider: string, model: string) => {
    await client.PUT('/models/default', { body: { provider, model } });
  },
  delete: async (id: string) => {
    await client.DELETE('/models/{modelID}', { params: { path: { modelID: id } } });
  },
  getProviders: async () => {
    const { data } = await client.GET('/providers');
    return (data?.data ?? []) as ProviderInfo[];
  },
  configureProvider: async (name: string, body: { api_key: string; base_url?: string }) => {
    await client.POST('/providers/{name}/configure', {
      params: { path: { name } },
      body: body as Schemas['ConfigureProviderRequest'],
    });
  },
};

// ── Tool API ──

export interface ToolItem {
  name: string;
  description: string;
}

export const toolApi = {
  list: async () => {
    const { data } = await client.GET('/tools');
    return (data?.data ?? []) as ToolItem[];
  },
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
  list: async () => {
    const { data } = await client.GET('/skills');
    return (data?.data ?? []) as SkillItem[];
  },
  install: async (body: Schemas['InstallSkillRequest']) => {
    const { data } = await client.POST('/skills/install', { body });
    return data as Schemas['SkillInstallResult'];
  },
  uninstall: async (name: string) => {
    await client.DELETE('/skills/{name}', { params: { path: { name } } });
  },
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

export type MonitoringTimeseriesPoint = {
  bucket_start: string;
  run_total?: number;
  run_success?: number;
  run_failed?: number;
  success_rate?: number;
  error_rate?: number;
  latency_p50_ms?: number;
  latency_p95_ms?: number;
  latency_p99_ms?: number;
  avg_elapsed_ms?: number;
  throughput_rpm?: number;
};

export type MonitoringRuntimeOverview = Schemas['RuntimeManagerStats'] & {
  current?: {
    runtime_id: string;
    actor_count: number;
    kanban_card_count: number;
    sandbox_leases: number;
  };
};

export type MonitoringTopFailedAgent = {
  agent_id: string;
  failed_runs: number;
  total_runs: number;
  failure_rate?: number;
};

export type MonitoringTopErrorCode = {
  code: string;
  count: number;
};

export type MonitoringRecentFailure = {
  run_id: string;
  agent_id: string;
  error_code: string;
  message: string;
  elapsed_ms: number;
  created_at: string;
};

export type MonitoringDiagnostics = {
  top_failed_agents: MonitoringTopFailedAgent[];
  top_error_codes: MonitoringTopErrorCode[];
  recent_failures: MonitoringRecentFailure[];
};

export const statsApi = {
  overview: async () => {
    const { data } = await client.GET('/stats');
    return data as StatsOverview;
  },
  runs: async (agentId?: string) => {
    const { data } = await client.GET('/stats/runs', {
      params: { query: agentId ? { agent_id: agentId } : {} },
    });
    return (data?.data ?? []) as RunStats[];
  },
  runtime: async () => {
    const { data } = await client.GET('/stats/runtime');
    return data as RuntimeStatsOverview;
  },
  memory: async () => {
    const { data } = await client.GET('/stats/memory');
    return data as MemoryStatsOverview;
  },
};

export const monitoringApi = {
  summary: async (params?: { window?: string; agentId?: string }) => {
    const { data } = await client.GET('/monitoring/summary', {
      params: {
        query: {
          ...(params?.window ? { window: params.window as '1h' | '6h' | '24h' | '7d' } : {}),
          ...(params?.agentId ? { agent_id: params.agentId } : {}),
        },
      },
    });
    return data as MonitoringSummary;
  },
  timeseries: async (params?: { window?: string; interval?: string; agentId?: string }) => {
    const { data } = await client.GET('/monitoring/timeseries', {
      params: {
        query: {
          ...(params?.window ? { window: params.window } : {}),
          ...(params?.interval ? { interval: params.interval as '1m' | '5m' | '15m' | '1h' } : {}),
          ...(params?.agentId ? { agent_id: params.agentId } : {}),
        },
      },
    });
    return (data?.data ?? []) as MonitoringTimeseriesPoint[];
  },
  runtime: async () => {
    const { data } = await client.GET('/monitoring/runtime');
    return data as MonitoringRuntimeOverview;
  },
  diagnostics: async (params?: { window?: string; agentId?: string; limit?: number }) => {
    const { data } = await client.GET('/monitoring/diagnostics', {
      params: {
        query: {
          ...(params?.window ? { window: params.window } : {}),
          ...(params?.agentId ? { agent_id: params.agentId } : {}),
          ...(params?.limit ? { limit: params.limit } : {}),
        },
      },
    });
    return data as MonitoringDiagnostics;
  },
};

// ── Workflow Run API ──

export const workflowRunApi = {
  list: async (agentId?: string) => {
    const { data } = await client.GET('/workflows/runs', {
      params: { query: agentId ? { agent_id: agentId } : {} },
    });
    return (data?.data ?? []) as WorkflowRun[];
  },
  get: async (id: string) => {
    const { data } = await client.GET('/workflows/runs/{id}', {
      params: { path: { id } },
    });
    return data as unknown as WorkflowRun;
  },
  status: async (id: string) => {
    const { data } = await client.GET('/workflows/runs/{id}/status', {
      params: { path: { id } },
    });
    return data as unknown as { status: string };
  },
  events: async (id: string) => {
    const { data } = await client.GET('/workflows/runs/{id}/events', {
      params: { path: { id } },
    });
    return (data?.data ?? []) as ExecutionEvent[];
  },
};

// ── Kanban API ──

export const kanbanApi = {
  cards: async () => {
    const { data } = await client.GET('/kanban/cards');
    return (data?.data ?? []) as unknown as KanbanSnapshot['cards'];
  },
  timeline: async () => {
    const { data } = await client.GET('/kanban/timeline');
    return (data?.data ?? []) as unknown as TimelineEntry[];
  },
  topology: async () => {
    const { data } = await client.GET('/kanban/topology');
    return data as unknown as { nodes: TopologyNode[]; edges: TopologyEdge[] };
  },
};

// ── Plugin API ──

export type PluginDetail = Schemas['PluginDetail'];

export const pluginApi = {
  list: async () => {
    const { data } = await client.GET('/plugins');
    return (data?.data ?? []) as unknown as Plugin[];
  },
  get: async (name: string) => {
    const { data } = await client.GET('/plugins/{name}', {
      params: { path: { name } },
    });
    return data as unknown as Plugin;
  },
  enable: async (name: string) => {
    const { data } = await client.POST('/plugins/{name}/enable', {
      params: { path: { name } },
    });
    return data as Schemas['PluginInfo'];
  },
  disable: async (name: string) => {
    const { data } = await client.POST('/plugins/{name}/disable', {
      params: { path: { name } },
    });
    return data as Schemas['PluginInfo'];
  },
  configure: async (name: string, config: Record<string, unknown>) => {
    const { data } = await client.PUT('/plugins/{name}/config', {
      params: { path: { name } },
      body: config,
    });
    return data as Schemas['PluginInfo'];
  },
  reload: async () => {
    const { data } = await client.POST('/plugins/reload');
    return data as { added?: string[]; removed?: string[] };
  },
  upload: async (file: File) => {
    const form = new FormData();
    form.append('file', file);
    const res = await fetch('/api/plugins/upload', {
      method: 'POST',
      body: form,
      credentials: 'include',
    });
    if (!res.ok) {
      const body = await res.json().catch(() => ({}));
      const err = body as { error?: { message?: string }; message?: string };
      throw new ApiError(res.status, err.error?.message || err.message || res.statusText);
    }
    return res.json() as Promise<Schemas['PluginUploadResult']>;
  },
  remove: async (name: string) => {
    await client.DELETE('/plugins/{name}', { params: { path: { name } } });
  },
};

// ── Channel Types API ──

export type ChannelTypeField = {
  type: string;
  required?: boolean;
  secret?: boolean;
};

export interface ChannelTypeSchema {
  type: string;
  label: string;
  config_schema: Record<string, ChannelTypeField>;
}

export const channelApi = {
  types: async () => {
    const { data } = await client.GET('/channel-types');
    return (data?.data ?? []) as ChannelTypeSchema[];
  },
};

// ── WebSocket Ticket ──

export interface WSTicket {
  ticket: string;
  expires_at: string;
}

export const wsApi = {
  ticket: async () => {
    const { data } = await client.POST('/ws-ticket');
    return data as WSTicket;
  },
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
  list: async (category?: string) => {
    const { data } = await client.GET('/memories', {
      params: { query: category ? { category } : {} },
    });
    return (data?.data ?? []) as MemoryEntry[];
  },
  update: async (entryId: string, content: string) => {
    const { data } = await client.PUT('/memories/{entryID}', {
      params: { path: { entryID: entryId } },
      body: { content },
    });
    return data as MemoryEntry;
  },
  delete: async (entryId: string) => {
    await client.DELETE('/memories/{entryID}', {
      params: { path: { entryID: entryId } },
    });
  },
};

// ── Setup API ──

export const setupApi = {
  getStatus: () =>
    modelApi.getProviders().then((providers) => ({
      status: providers.some((p) => p.configured) ? ('configured' as const) : ('not_configured' as const),
    })),
};
