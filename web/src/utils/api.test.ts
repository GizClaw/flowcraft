import { describe, it, expect, vi, beforeEach } from 'vitest';
import { apiStream, toolApi } from './api';
import client from '../api/client';

vi.mock('../store/toastStore', () => ({
  useToastStore: {
    getState: () => ({ addToast: vi.fn() }),
  },
}));

function mockFetchSSE(chunks: string[]) {
  let idx = 0;
  const reader = {
    read: vi.fn().mockImplementation(() => {
      if (idx >= chunks.length) return Promise.resolve({ done: true, value: undefined });
      const value = new TextEncoder().encode(chunks[idx++]);
      return Promise.resolve({ done: false, value });
    }),
    releaseLock: vi.fn(),
  };

  return vi.fn().mockResolvedValue({
    ok: true,
    status: 200,
    body: { getReader: () => reader },
  });
}

beforeEach(() => {
  vi.restoreAllMocks();
});

describe('apiStream', () => {
  it('parses SSE events with event type', async () => {
    globalThis.fetch = mockFetchSSE([
      'event: agent_token\ndata: {"chunk":"hello"}\n\n',
      'event: done\ndata: {"output":{}}\n\n',
    ]);

    const events: unknown[] = [];
    for await (const event of apiStream('/api/stream', { method: 'POST', json: {} })) {
      events.push(event);
    }

    expect(events).toHaveLength(2);
    expect(events[0]).toEqual({ type: 'agent_token', chunk: 'hello' });
    expect(events[1]).toEqual({ type: 'done', output: {} });
  });

  it('handles data-only events (no event: line)', async () => {
    globalThis.fetch = mockFetchSSE([
      'data: {"type":"node_start","node_id":"n1"}\n\n',
    ]);

    const events: unknown[] = [];
    for await (const event of apiStream('/api/stream')) {
      events.push(event);
    }

    expect(events).toHaveLength(1);
    expect(events[0]).toEqual({ type: 'node_start', node_id: 'n1' });
  });

  it('handles chunked SSE data split across reads', async () => {
    globalThis.fetch = mockFetchSSE([
      'event: agent_token\nda',
      'ta: {"chunk":"split"}\n\n',
    ]);

    const events: unknown[] = [];
    for await (const event of apiStream('/api/stream')) {
      events.push(event);
    }

    expect(events).toHaveLength(1);
    expect(events[0]).toEqual({ type: 'agent_token', chunk: 'split' });
  });

  it('skips malformed JSON lines', async () => {
    globalThis.fetch = mockFetchSSE([
      'data: not-json\n\n',
      'data: {"valid":true}\n\n',
    ]);

    const events: unknown[] = [];
    for await (const event of apiStream('/api/stream')) {
      events.push(event);
    }

    expect(events).toHaveLength(1);
    expect(events[0]).toEqual({ valid: true });
  });

  it('throws on non-ok response', async () => {
    globalThis.fetch = vi.fn().mockResolvedValue({
      ok: false,
      status: 500,
      statusText: 'Internal Server Error',
      json: () => Promise.resolve({ message: 'boom' }),
    });

    const events: unknown[] = [];
    await expect(async () => {
      for await (const event of apiStream('/api/stream')) {
        events.push(event);
      }
    }).rejects.toThrow('boom');
  });

  it('handles empty body gracefully', async () => {
    globalThis.fetch = vi.fn().mockResolvedValue({
      ok: true,
      status: 200,
      body: null,
    });

    const events: unknown[] = [];
    for await (const event of apiStream('/api/stream')) {
      events.push(event);
    }

    expect(events).toHaveLength(0);
  });

  it('resets event type on blank line', async () => {
    globalThis.fetch = mockFetchSSE([
      'event: agent_token\n\ndata: {"chunk":"no-type"}\n\n',
    ]);

    const events: unknown[] = [];
    for await (const event of apiStream('/api/stream')) {
      events.push(event);
    }

    expect(events).toHaveLength(1);
    expect((events[0] as Record<string, unknown>).type).toBeUndefined();
    expect((events[0] as Record<string, unknown>).chunk).toBe('no-type');
  });
});

describe('toolApi', () => {
  it('list returns tool items', async () => {
    const tools = [
      { name: 'sandbox_bash', description: 'Run commands' },
      { name: 'skill', description: 'Search, inspect, and execute skills' },
    ];
    vi.spyOn(client, 'GET').mockResolvedValue({
      data: { data: tools },
      error: undefined,
      response: new Response(),
    } as never);

    const result = await toolApi.list();
    expect(result).toEqual(tools);
    expect(result).toHaveLength(2);
    expect(result[0].name).toBe('sandbox_bash');
  });

  it('list returns empty array when data is null', async () => {
    vi.spyOn(client, 'GET').mockResolvedValue({
      data: { data: null },
      error: undefined,
      response: new Response(),
    } as never);

    const result = await toolApi.list();
    expect(result).toEqual([]);
  });

  it('list calls GET /tools', async () => {
    const spy = vi.spyOn(client, 'GET').mockResolvedValue({
      data: { data: [] },
      error: undefined,
      response: new Response(),
    } as never);

    await toolApi.list();

    expect(spy).toHaveBeenCalledWith('/tools');
  });
});
