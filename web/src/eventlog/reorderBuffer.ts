// ReorderBuffer: 200ms cross-partition reorder window.
//
// Same-partition order is guaranteed by the backend; this buffer only
// addresses micro-jitter between partitions that the doc explicitly allows
// (e.g. card:* + webhook:*). Algorithm:
//   - Push every envelope into a min-heap keyed by (seq, arrivedAt).
//   - On each push, schedule a flush so that any envelope older than
//     `windowMs` from arrivedAt is forwarded to the router in seq order.
//   - Cap the buffer at 1000 entries; on overflow, drop the oldest and
//     emit a console warning so we surface back-pressure quickly.

import type { Envelope } from './types';

interface Entry {
  env: Envelope;
  arrivedAt: number;
}

export interface ReorderOptions {
  windowMs?: number;
  maxEntries?: number;
  onFlush: (env: Envelope) => void;
  now?: () => number;
}

export class ReorderBuffer {
  private opts: Required<Omit<ReorderOptions, 'now'>> & { now: () => number };
  private buf: Entry[] = [];
  private timer: ReturnType<typeof setTimeout> | null = null;

  constructor(opts: ReorderOptions) {
    this.opts = {
      windowMs: opts.windowMs ?? 200,
      maxEntries: opts.maxEntries ?? 1000,
      onFlush: opts.onFlush,
      now: opts.now ?? (() => Date.now()),
    };
  }

  push(env: Envelope): void {
    this.buf.push({ env, arrivedAt: this.opts.now() });
    this.buf.sort((a, b) => a.env.seq - b.env.seq);
    if (this.buf.length > this.opts.maxEntries) {
      const dropped = this.buf.shift();
      if (dropped) {
        console.warn('reorderBuffer: dropping envelope, buffer at capacity', {
          seq: dropped.env.seq,
        });
      }
    }
    this.scheduleFlush();
  }

  flushAll(): void {
    if (this.timer) {
      clearTimeout(this.timer);
      this.timer = null;
    }
    while (this.buf.length > 0) {
      const { env } = this.buf.shift()!;
      this.opts.onFlush(env);
    }
  }

  private scheduleFlush(): void {
    if (this.timer) return;
    this.timer = setTimeout(() => {
      this.timer = null;
      this.drain();
    }, this.opts.windowMs);
  }

  private drain(): void {
    const cutoff = this.opts.now() - this.opts.windowMs;
    while (this.buf.length > 0 && this.buf[0].arrivedAt <= cutoff) {
      const { env } = this.buf.shift()!;
      this.opts.onFlush(env);
    }
    if (this.buf.length > 0) this.scheduleFlush();
  }
}
