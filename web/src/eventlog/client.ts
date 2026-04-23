// EnvelopeClient: the singleton transport that bridges the backend
// envelope log into the frontend reducer surface.
//
// Strategy (R5 §6.6):
//   - Prefer WebSocket (/api/events/ws) for live tailing
//   - Fall back to SSE   (/api/events/stream) on 3 consecutive WS failures
//   - Fall back to HTTP pull (/api/events) on 3 consecutive SSE failures
//   - Auto-recover: every reconnect attempt re-tries the higher tier first
//
// Re-emits two channels:
//   - `envelope`  – every envelope routed through the active transport
//   - `heartbeat` – server-side latest-seq pings (used to detect drift)
//
// All envelopes are pushed through `envelopeRouter.dispatch()` so business
// reducers stay transport-agnostic. The cross-partition reorder buffer is
// applied to partitions where the doc allows micro-ordering jitter
// (`card:` / `webhook:`); other partitions go straight to the router.

import type { Envelope, PullResponse } from './types';
import { envelopeRouter } from './router';
import { ReorderBuffer } from './reorderBuffer';

export type ConnectionState =
  | 'connecting'
  | 'connected'
  | 'reconnecting'
  | 'disconnected';

export type EnvelopeClientEvent = 'connected' | 'disconnected' | 'envelope' | 'heartbeat';
export type Listener<T> = (payload: T) => void;
export type UnsubscribeFn = () => void;

export interface EnvelopeClientOptions {
  baseUrl?: string;
  pollIntervalMs?: number;
  reorderWindowMs?: number;
  reorderPartitions?: string[]; // prefixes that go through the reorder buffer
  fetchImpl?: typeof fetch;
  websocketImpl?: typeof WebSocket;
  eventSourceImpl?: typeof EventSource;
}

interface SubscriptionState {
  partition: string;
  since: number;
}

const REORDER_DEFAULT_PREFIXES = ['card:', 'webhook:'];

export class EnvelopeClient {
  private opts: Required<EnvelopeClientOptions>;
  private state: ConnectionState = 'disconnected';
  private subs = new Map<string, SubscriptionState>();
  private listeners: Record<EnvelopeClientEvent, Set<Listener<unknown>>> = {
    connected: new Set(),
    disconnected: new Set(),
    envelope: new Set(),
    heartbeat: new Set(),
  };
  private reorderBuffer: ReorderBuffer;
  private latest = 0;
  private lastFrameAt = 0;
  private wsFailures = 0;
  private sseFailures = 0;
  private currentWS: WebSocket | null = null;
  private currentSSE: EventSource | null = null;
  private currentPullTimer: ReturnType<typeof setTimeout> | null = null;

  constructor(opts: EnvelopeClientOptions = {}) {
    this.opts = {
      baseUrl: opts.baseUrl ?? '',
      pollIntervalMs: opts.pollIntervalMs ?? 5000,
      reorderWindowMs: opts.reorderWindowMs ?? 200,
      reorderPartitions: opts.reorderPartitions ?? REORDER_DEFAULT_PREFIXES,
      fetchImpl: opts.fetchImpl ?? globalThis.fetch?.bind(globalThis),
      websocketImpl: opts.websocketImpl ?? (globalThis as { WebSocket?: typeof WebSocket }).WebSocket!,
      eventSourceImpl: opts.eventSourceImpl ?? (globalThis as { EventSource?: typeof EventSource }).EventSource!,
    };
    this.reorderBuffer = new ReorderBuffer({
      windowMs: this.opts.reorderWindowMs,
      onFlush: (env) => this.dispatchToRouter(env),
    });
  }

  subscribe(partition: string, since = 0): UnsubscribeFn {
    const existing = this.subs.get(partition);
    const next: SubscriptionState = {
      partition,
      since: existing ? Math.max(existing.since, since) : since,
    };
    this.subs.set(partition, next);
    if (this.state === 'disconnected') {
      this.connect();
    } else if (this.currentWS) {
      this.sendWSSubscribe(partition, next.since);
    }
    return () => {
      this.subs.delete(partition);
      if (this.subs.size === 0) {
        this.disconnect();
      }
    };
  }

  async pull(partition: string, since: number, limit = 200): Promise<PullResponse> {
    const url = `${this.opts.baseUrl}/api/events?partition=${encodeURIComponent(partition)}&since=${since}&limit=${limit}`;
    const res = await this.opts.fetchImpl(url, { credentials: 'include' });
    if (!res.ok) throw new Error(`pull ${partition}: ${res.status}`);
    return (await res.json()) as PullResponse;
  }

  async fetchLatestSeq(): Promise<number> {
    const url = `${this.opts.baseUrl}/api/events/latest-seq`;
    const res = await this.opts.fetchImpl(url, { credentials: 'include' });
    if (!res.ok) throw new Error(`latest-seq: ${res.status}`);
    const data = (await res.json()) as { latest_seq: number };
    this.latest = data.latest_seq ?? 0;
    return this.latest;
  }

  on<T = unknown>(event: EnvelopeClientEvent, handler: Listener<T>): UnsubscribeFn {
    this.listeners[event].add(handler as Listener<unknown>);
    return () => this.listeners[event].delete(handler as Listener<unknown>);
  }

  latestSeq(): number {
    return this.latest;
  }

  connectionState(): ConnectionState {
    return this.state;
  }

  // --- transport wiring ---

  private connect(): void {
    if (this.state === 'connected' || this.state === 'connecting') return;
    if (this.subs.size === 0) return;
    this.state = 'connecting';
    this.tryWebSocket();
  }

  private async tryWebSocket(): Promise<void> {
    if (!this.opts.websocketImpl || !this.opts.fetchImpl) {
      return this.tryEventSource();
    }
    if (this.subs.size === 0) {
      return; // no partitions to subscribe to yet
    }
    try {
      // §12.3: each WS connect issues a one-shot ticket bound to one
      // (partition, since) pair. Pick the first active subscription as
      // the initial — additional partitions are joined post-connect via
      // sendWSSubscribe (one connection multiplexes many subs).
      const initial = this.subs.values().next().value as SubscriptionState;
      const ticketRes = await this.opts.fetchImpl(`${this.opts.baseUrl}/api/ws-ticket`, {
        method: 'POST',
        credentials: 'include',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ partition: initial.partition, since: initial.since }),
      });
      if (!ticketRes.ok) throw new Error(`ws-ticket: ${ticketRes.status}`);
      const { ticket } = (await ticketRes.json()) as { ticket: string };
      const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
      const url = `${proto}//${location.host}${this.opts.baseUrl}/api/events/ws?ticket=${encodeURIComponent(ticket)}`;
      const ws = new this.opts.websocketImpl(url);
      this.currentWS = ws;
      ws.onopen = () => {
        this.wsFailures = 0;
        this.markConnected();
        // Initial subscribe is auto-applied server-side from the ticket;
        // we explicitly subscribe any additional partitions.
        this.subs.forEach((s) => {
          if (s.partition !== initial.partition) {
            this.sendWSSubscribe(s.partition, s.since);
          }
        });
      };
      ws.onmessage = (msg) => this.handleRawFrame(msg.data);
      ws.onerror = () => this.handleWSFailure();
      ws.onclose = () => this.handleWSFailure();
    } catch {
      this.handleWSFailure();
    }
  }

  private sendWSSubscribe(partition: string, since: number): void {
    if (!this.currentWS || this.currentWS.readyState !== this.currentWS.OPEN) return;
    this.currentWS.send(JSON.stringify({ type: 'subscribe', partition, since }));
  }

  private handleWSFailure(): void {
    this.wsFailures += 1;
    this.currentWS = null;
    if (this.wsFailures >= 3) {
      this.tryEventSource();
    } else {
      this.scheduleReconnect(() => this.tryWebSocket());
    }
  }

  private tryEventSource(): void {
    if (!this.opts.eventSourceImpl || this.subs.size === 0) {
      return this.startHTTPPull();
    }
    // SSE only carries one partition per connection in this implementation;
    // we open one stream per active subscription.
    this.subs.forEach((sub) => {
      const url = `${this.opts.baseUrl}/api/events/stream?partition=${encodeURIComponent(sub.partition)}&since=${sub.since}`;
      try {
        const es = new this.opts.eventSourceImpl(url, { withCredentials: true });
        this.currentSSE = es;
        es.onopen = () => this.markConnected();
        es.addEventListener('envelope', (ev) => this.handleRawFrame((ev as MessageEvent).data));
        es.addEventListener('heartbeat', (ev) => this.handleHeartbeat((ev as MessageEvent).data));
        es.onerror = () => this.handleSSEFailure();
      } catch {
        this.handleSSEFailure();
      }
    });
  }

  private handleSSEFailure(): void {
    this.sseFailures += 1;
    if (this.currentSSE) {
      try {
        this.currentSSE.close();
      } catch {
        /* ignore */
      }
      this.currentSSE = null;
    }
    if (this.sseFailures >= 3) {
      this.startHTTPPull();
    } else {
      this.scheduleReconnect(() => this.tryEventSource());
    }
  }

  private startHTTPPull(): void {
    if (this.subs.size === 0) return;
    this.markConnected();
    const tick = async () => {
      for (const sub of this.subs.values()) {
        try {
          const page = await this.pull(sub.partition, sub.since);
          page.events.forEach((env) => this.handleEnvelope(env));
          if (page.next_since > sub.since) sub.since = page.next_since;
        } catch {
          /* swallow; next tick will retry */
        }
      }
      this.currentPullTimer = setTimeout(tick, this.opts.pollIntervalMs);
    };
    tick();
  }

  private handleRawFrame(raw: string): void {
    try {
      const env = JSON.parse(raw) as Envelope;
      this.handleEnvelope(env);
    } catch (err) {
      console.warn('envelopeClient: invalid frame', err);
    }
  }

  private handleHeartbeat(raw: string): void {
    try {
      const data = JSON.parse(raw) as { latest_seq?: number; ts?: string };
      if (data.latest_seq && data.latest_seq > this.latest) this.latest = data.latest_seq;
      this.lastFrameAt = Date.now();
      this.listeners.heartbeat.forEach((fn) => fn(data));
    } catch {
      /* ignore */
    }
  }

  private handleEnvelope(env: Envelope): void {
    if (env.seq > this.latest) this.latest = env.seq;
    this.lastFrameAt = Date.now();
    const sub = this.subs.get(env.partition);
    if (sub && env.seq > sub.since) sub.since = env.seq;
    this.listeners.envelope.forEach((fn) => fn(env));
    if (this.shouldReorder(env.partition)) {
      this.reorderBuffer.push(env);
    } else {
      this.dispatchToRouter(env);
    }
  }

  private shouldReorder(partition: string): boolean {
    return this.opts.reorderPartitions.some((prefix) => partition.startsWith(prefix));
  }

  private dispatchToRouter(env: Envelope): void {
    envelopeRouter.dispatch(env);
  }

  private markConnected(): void {
    if (this.state === 'connected') return;
    this.state = 'connected';
    this.listeners.connected.forEach((fn) => fn(undefined));
  }

  private scheduleReconnect(retry: () => void): void {
    this.state = 'reconnecting';
    const base = Math.min(30_000, 1000 * Math.pow(2, this.wsFailures + this.sseFailures));
    const jitter = Math.floor((Math.random() - 0.5) * base * 0.3);
    setTimeout(retry, base + jitter);
  }

  private disconnect(): void {
    if (this.state === 'disconnected') return;
    this.state = 'disconnected';
    if (this.currentWS) {
      try {
        this.currentWS.close();
      } catch {
        /* ignore */
      }
      this.currentWS = null;
    }
    if (this.currentSSE) {
      try {
        this.currentSSE.close();
      } catch {
        /* ignore */
      }
      this.currentSSE = null;
    }
    if (this.currentPullTimer) {
      clearTimeout(this.currentPullTimer);
      this.currentPullTimer = null;
    }
    this.reorderBuffer.flushAll();
    this.listeners.disconnected.forEach((fn) => fn(undefined));
  }
}

let singleton: EnvelopeClient | null = null;

export function getEnvelopeClient(): EnvelopeClient {
  if (!singleton) singleton = new EnvelopeClient();
  return singleton;
}

// Test-only: replace the singleton (lets unit tests inject mocks without
// poisoning shared state across test files).
export function _setEnvelopeClientForTest(client: EnvelopeClient | null): void {
  singleton = client;
}
