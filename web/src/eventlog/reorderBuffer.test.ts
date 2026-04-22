import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { ReorderBuffer } from './reorderBuffer';
import type { Envelope } from './types';

function envOf(seq: number, partition = 'card:1'): Envelope {
  return {
    seq,
    partition,
    type: 'task.submitted',
    version: 1,
    category: 'business',
    ts: '2026-04-22T00:00:00Z',
    payload: {},
  };
}

describe('ReorderBuffer', () => {
  beforeEach(() => {
    vi.useFakeTimers();
  });
  afterEach(() => {
    vi.useRealTimers();
  });

  it('flushes envelopes in seq order after the window expires', () => {
    const flushed: number[] = [];
    const buf = new ReorderBuffer({
      windowMs: 200,
      onFlush: (env) => flushed.push(env.seq),
    });
    buf.push(envOf(2));
    buf.push(envOf(1));
    expect(flushed).toEqual([]);
    vi.advanceTimersByTime(199);
    expect(flushed).toEqual([]);
    vi.advanceTimersByTime(2);
    expect(flushed).toEqual([1, 2]);
  });

  it('flushAll drains immediately', () => {
    const flushed: number[] = [];
    const buf = new ReorderBuffer({
      windowMs: 200,
      onFlush: (env) => flushed.push(env.seq),
    });
    buf.push(envOf(5));
    buf.push(envOf(4));
    buf.flushAll();
    expect(flushed).toEqual([4, 5]);
  });

  it('drops the oldest entry when capacity is exceeded', () => {
    const flushed: number[] = [];
    const buf = new ReorderBuffer({
      windowMs: 50,
      maxEntries: 2,
      onFlush: (env) => flushed.push(env.seq),
    });
    buf.push(envOf(10));
    buf.push(envOf(11));
    buf.push(envOf(12));
    expect(buf['buf'].length).toBeLessThanOrEqual(2);
  });
});
