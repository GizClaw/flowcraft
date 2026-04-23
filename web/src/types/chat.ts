export interface Message {
  id: string;
  conversation_id: string;
  role: 'user' | 'assistant' | 'system' | 'tool';
  content: string;
  metadata?: Record<string, unknown>;
  token_count?: number;
  created_at: string;
}

export interface ToolCallInfo {
  id?: string;
  name: string;
  args: string;
  result?: string;
  status: 'pending' | 'success' | 'error';
}

export interface DispatchedTask {
  cardId: string;
  template: string;
  status: 'submitted' | 'running' | 'success' | 'error';
}

export interface RichMessage {
  id: string;
  role: 'user' | 'assistant';
  content: string;
  toolCalls?: ToolCallInfo[];
  dispatchedTask?: DispatchedTask;
  isCallback?: boolean;
  cardId?: string;
  timestamp: string;
}

export interface Conversation {
  id: string;
  agent_id: string;
  runtime_id: string;
  variables?: Record<string, unknown>;
  status: 'active' | 'closed' | 'archived';
  created_at: string;
  updated_at: string;
}

export interface TokenUsage {
  input_tokens: number;
  output_tokens: number;
  total_tokens: number;
}

// ChatRequest is the body of POST /api/conversations/{id}/runs. The
// conversation id lives in the URL path, not the body.
export interface ChatRequest {
  agent_id: string;
  query: string;
  inputs?: Record<string, unknown>;
  async?: boolean;
}

export interface CoPilotContextInput {
  current_agent_id?: string;
  refs?: CoPilotRef[];
  graph_context?: GraphContextInput;
}

export interface ChatResponse {
  conversation_id: string;
  message_id: string;
  answer: string;
  metadata?: Record<string, unknown>;
  usage?: TokenUsage;
  elapsed_ms: number;
  status?: 'completed' | 'interrupted';
  run_id?: string;
  state?: Record<string, unknown>;
}


interface StreamEventBase {
  run_id?: string;
  graph_id?: string;
  node_id?: string;
  timestamp?: string;
  data?: Record<string, unknown>;
}

export interface GraphStartEvent extends StreamEventBase { type: 'graph_start' }
export interface GraphEndEvent extends StreamEventBase { type: 'graph_end'; output?: ChatResponse }
export interface NodeStartEvent extends StreamEventBase { type: 'node_start' }
export interface NodeCompleteEvent extends StreamEventBase { type: 'node_complete' }
export interface NodeSkippedEvent extends StreamEventBase { type: 'node_skipped' }
export interface NodeErrorEvent extends StreamEventBase { type: 'node_error'; error?: string }
export interface AgentTokenEvent extends StreamEventBase { type: 'agent_token'; chunk?: string }
export interface AgentToolCallEvent extends StreamEventBase { type: 'agent_tool_call'; tool_call_id?: string; tool_name?: string; tool_args?: string }
export interface AgentToolResultEvent extends StreamEventBase { type: 'agent_tool_result'; tool_call_id?: string; tool_name?: string; tool_result?: string; is_error?: boolean }
export interface ParallelForkEvent extends StreamEventBase { type: 'parallel_fork' }
export interface ParallelJoinEvent extends StreamEventBase { type: 'parallel_join' }
export interface CheckpointEvent extends StreamEventBase { type: 'checkpoint' }
export interface KanbanUpdateEvent extends StreamEventBase {
  type: 'kanban_update';
  event_type?: string;
  payload?: { card_id?: string; output?: string; error?: string };
}
export interface ApprovalRequiredEvent extends StreamEventBase {
  type: 'approval_required';
  prompt?: string;
  conversation_id?: string;
}
export interface StreamErrorEvent extends StreamEventBase { type: 'error'; error?: string; message?: string; code?: string }
export interface StreamDoneEvent extends StreamEventBase { type: 'done'; output?: ChatResponse; status?: string; conversation_id?: string }

export type WorkflowStreamEvent =
  | GraphStartEvent
  | GraphEndEvent
  | NodeStartEvent
  | NodeCompleteEvent
  | NodeSkippedEvent
  | NodeErrorEvent
  | AgentTokenEvent
  | AgentToolCallEvent
  | AgentToolResultEvent
  | ParallelForkEvent
  | ParallelJoinEvent
  | CheckpointEvent
  | KanbanUpdateEvent
  | ApprovalRequiredEvent
  | StreamErrorEvent
  | StreamDoneEvent;

export interface CoPilotRef {
  type: 'node';
  id: string;
}

export interface GraphContextInput {
  node_count: number;
  edge_count: number;
  node_types: Record<string, number>;
  summary: string;
}

export interface WorkflowRun {
  id: string;
  agent_id: string;
  actor_id?: string;
  conversation_id?: string;
  input?: string;
  output?: string;
  inputs?: Record<string, unknown>;
  outputs?: Record<string, unknown>;
  status: 'running' | 'completed' | 'failed' | 'interrupted' | 'timeout';
  elapsed_ms: number;
  created_at: string;
}

