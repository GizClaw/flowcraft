export type CardStatus = 'pending' | 'claimed' | 'done' | 'failed';

// UI-friendly status names used in frontend (copilotStore.DispatchedTask)
export type UIStatus = 'submitted' | 'running' | 'success' | 'error';

/**
 * Maps backend KanbanCard status to frontend UI status.
 *
 * Backend (KanbanCard.status): 'pending' | 'claimed' | 'done' | 'failed'
 * Frontend (DispatchedTask.status): 'submitted' | 'running' | 'success' | 'error'
 *
 * Mapping:
 *   - 'pending' -> 'submitted'  (task created, not yet picked up)
 *   - 'claimed' -> 'running'    (agent is working on task)
 *   - 'done' -> 'success'       (task completed successfully)
 *   - 'failed' -> 'error'       (task failed)
 */
export function mapCardStatusToUI(status: CardStatus): UIStatus {
  const mapping: Record<CardStatus, UIStatus> = {
    pending: 'submitted',
    claimed: 'running',
    done: 'success',
    failed: 'error',
  };
  return mapping[status];
}

/**
 * Maps frontend UI status back to backend KanbanCard status.
 * Used when syncing status changes back to kanban.
 */
export function mapUIStatusToCard(status: UIStatus): CardStatus {
  const mapping: Record<UIStatus, CardStatus> = {
    submitted: 'pending',
    running: 'claimed',
    success: 'done',
    error: 'failed',
  };
  return mapping[status];
}

export interface KanbanCard {
  id: string;
  type: string;
  status: CardStatus;
  producer: string;
  consumer: string;
  query?: string;
  template?: string;
  target_agent_id?: string;
  output?: string;
  error?: string;
  run_id?: string;
  created_at: string;
  updated_at: string;
  elapsed_ms?: number;
  meta?: Record<string, string>;
  // R5 §13: realm that owns the runtime this card lives in. Optional so
  // legacy snapshot consumers (and tests that synthesise cards) keep
  // working without filling it in.
  realm_id?: string;
}

export interface KanbanEvent {
  type: 'card_created' | 'card_claimed' | 'card_done' | 'card_failed';
  card?: KanbanCard;
  timestamp: string;
}

export interface KanbanSnapshot {
  cards: KanbanCard[];
}

export interface TimelineEntry {
  card_id: string;
  type: string;
  status: CardStatus;
  agent_id?: string;
  query?: string;
  template?: string;
  target_agent_id?: string;
  created_at: string;
  updated_at: string;
  elapsed_ms?: number;
  error?: string;
}

export interface TopologyNode {
  id: string;
  name: string;
  type: string;
}

export interface TopologyEdge {
  source: string;
  target: string;
  card_id?: string;
  type?: string;
}
