import type { VariableSchema } from './variable';

export type AgentType = 'workflow' | 'copilot';

export interface ParallelConfig {
  enabled: boolean;
  max_branches?: number;
  max_nesting?: number;
  merge_strategy?: 'last_wins' | 'namespace' | 'error_on_conflict';
}

export interface NotificationConfig {
  enabled: boolean;
  channel_name?: string;
  granularity?: 'all' | 'final' | 'failure';
}

export interface LongTermConfig {
  enabled: boolean;
  categories?: string[];
  max_entries?: number;
  /** When true, long-term list/search use MemoryScope (runtime + conversation isolation). */
  scope_enabled?: boolean;
  /** Categories stored in the runtime-global bucket; empty uses server defaults. */
  global_categories?: string[];
  /** Always-injected categories; empty uses server defaults (e.g. profile, preferences). */
  pinned_categories?: string[];
  /** Query-retrieved categories; empty uses server defaults. */
  recall_categories?: string[];
}

export interface LosslessConfig {
  chunk_size?: number;
  condense_threshold?: number;
  max_depth?: number;
  token_budget?: number;
  recent_ratio?: number;
  compact_threshold?: number;
  prune_leaf_content?: boolean;
  archive_threshold?: number;
  archive_batch_size?: number;
}

export interface MemoryConfig {
  max_messages?: number;
  long_term?: LongTermConfig;
  lossless?: LosslessConfig;
}

export interface ChannelBinding {
  type: string;
  config: Record<string, string>;
}

export interface AgentConfig {
  skill_whitelist?: string[];
  memory?: MemoryConfig;
  parallel?: ParallelConfig;
  notification?: NotificationConfig;
  channels?: ChannelBinding[];
  [key: string]: unknown;
}

export interface GraphDefinition {
  name: string;
  entry: string;
  nodes: NodeDefinition[];
  edges: EdgeDefinition[];
}

export interface NodeDefinition {
  id: string;
  type: string;
  config?: Record<string, unknown>;
  skip_condition?: string;
}

export interface EdgeDefinition {
  from: string;
  to: string;
  condition?: string;
}

export interface Agent {
  id: string;
  name: string;
  type: AgentType;
  description?: string;
  config: AgentConfig;
  graph_definition?: GraphDefinition;
  input_schema?: VariableSchema;
  output_schema?: VariableSchema;
  created_at: string;
  updated_at: string;
}

export interface CreateAgentRequest {
  name: string;
  type: AgentType;
  description?: string;
  config?: AgentConfig;
  graph_definition?: GraphDefinition;
  template?: string;
}

export interface UpdateAgentRequest {
  name?: string;
  description?: string;
  config?: AgentConfig;
  graph_definition?: GraphDefinition;
  input_schema?: VariableSchema;
  output_schema?: VariableSchema;
}

export interface TemplateParameter {
  name: string;
  label: string;
  type: string;
  default_value?: unknown;
  required?: boolean;
  options?: { value: string; label: string }[];
  placeholder?: string;
}

export interface GraphTemplate {
  name: string;
  label: string;
  description: string;
  category: string;
  parameters?: TemplateParameter[];
  graph_def: GraphDefinition;
}

export interface CompileWarning {
  code: string;
  message: string;
  node_ids?: string[];
}

export interface CompileResult {
  success: boolean;
  errors?: CompileWarning[];
  warnings?: CompileWarning[];
  metadata?: Record<string, unknown>;
}

export interface DryRunNodeResult {
  node_id: string;
  node_type?: string;
  valid: boolean;
  warnings?: string[];
}

export interface DryRunWarningGroup {
  code: string;
  message: string;
  node_ids?: string[];
}

export interface DryRunResult {
  valid: boolean;
  node_results?: DryRunNodeResult[];
  warnings?: DryRunWarningGroup[];
}

/** @deprecated Use DryRunNodeResult instead */
export interface DryRunItem {
  node_id: string;
  level: 'error' | 'warning' | 'info';
  message: string;
}
