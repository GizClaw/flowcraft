import { describe, it, expect, vi } from 'vitest';
import { EnvelopeRouter } from './router';
import type { Envelope } from './types';

function env(type: string): Envelope {
  return {
    seq: 1,
    partition: 'runtime:rt-1',
    type,
    version: 1,
    category: 'business',
    ts: '2026-01-01T00:00:00Z',
    payload: {},
  };
}

describe('EnvelopeRouter', () => {
  it('dispatches by type to all reducers', () => {
    const router = new EnvelopeRouter();
    const a = vi.fn();
    const b = vi.fn();
    router.on('task.submitted', a);
    router.on('task.submitted', b);
    router.dispatch(env('task.submitted'));
    expect(a).toHaveBeenCalledOnce();
    expect(b).toHaveBeenCalledOnce();
  });

  it('does not invoke reducers for other types', () => {
    const router = new EnvelopeRouter();
    const fn = vi.fn();
    router.on('task.claimed', fn);
    router.dispatch(env('task.submitted'));
    expect(fn).not.toHaveBeenCalled();
  });

  it('unsubscribe stops further dispatches', () => {
    const router = new EnvelopeRouter();
    const fn = vi.fn();
    const off = router.on('task.submitted', fn);
    router.dispatch(env('task.submitted'));
    off();
    router.dispatch(env('task.submitted'));
    expect(fn).toHaveBeenCalledOnce();
    expect(router.hasReducer('task.submitted')).toBe(false);
  });

  it('reducer errors do not break the loop', () => {
    const router = new EnvelopeRouter();
    const ok = vi.fn();
    router.on('task.submitted', () => {
      throw new Error('boom');
    });
    router.on('task.submitted', ok);
    router.dispatch(env('task.submitted'));
    expect(ok).toHaveBeenCalledOnce();
  });
});
