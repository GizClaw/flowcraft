// Wire-format envelope shared by WS / SSE / HTTP pull. Mirrors
// internal/eventlog/envelope.go::Envelope; do not add fields here unless
// they also appear in contracts/events.yaml.
export interface Envelope<P = unknown> {
  seq: number;
  partition: string;
  type: string;
  version: number;
  category: string;
  ts: string;
  payload: P;
  actor?: { id: string; kind: string; realm_id?: string };
  trace_id?: string;
  span_id?: string;
}

export interface PullResponse {
  events: Envelope[];
  next_since: number;
  has_more: boolean;
}
