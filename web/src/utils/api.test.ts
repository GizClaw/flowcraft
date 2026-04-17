import { describe, it, expect, vi, beforeEach } from 'vitest';
import { apiFetch, apiStream, toolApi } from './api';

// Mock useAuthStore to avoid import side-effects
vi.mock('../store/authStore', () => ({
  useAuthStore: {
    getState: () => ({ authenticated: false }),
  },
}));

function mockFetchJson(data: unknown, status = 200) {
  return vi.fn().mockResolvedValue({
    ok: status >= 200 && status < 300,
    status,
    statusText: 'OK',
    headers: new Headers(),
    json: () => Promise.resolve(data),
  });
}

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

describe('apiFetch', () => {
  it('returns parsed JSON on success', async () => {
    globalThis.fetch = mockFetchJson({ id: '1', name: 'test' });
    const result = await apiFetch<{ id: string; name: string }>('/api/test');
    expect(result).toEqual({ id: '1', name: 'test' });
  });

  it('returns undefined for 204 No Content', async () => {
    globalThis.fetch = vi.fn().mockResolvedValue({
      ok: true,
      status: 204,
      statusText: 'No Content',
      headers: new Headers(),
      json: () => Promise.resolve(null),
    });
    const result = await apiFetch('/api/test');
    expect(result).toBeUndefined();
  });

  it('throws error with message from response body', async () => {
    globalThis.fetch = vi.fn().mockResolvedValue({
      ok: false,
      status: 400,
      statusText: 'Bad Request',
      json: () => Promise.resolve({ error: { message: 'invalid input' } }),
    });
    await expect(apiFetch('/api/bad')).rejects.toThrow('invalid input');
  });

  it('falls back to statusText if no error message', async () => {
    globalThis.fetch = vi.fn().mockResolvedValue({
      ok: false,
      status: 500,
      statusText: 'Internal Server Error',
      json: () => Promise.reject(new Error('not json')),
    });
    await expect(apiFetch('/api/fail')).rejects.toThrow('Internal Server Error');
  });

  it('sends JSON body and content-type header', async () => {
    globalThis.fetch = mockFetchJson({ ok: true });
    await apiFetch('/api/test', { method: 'POST', json: { key: 'value' } });

    const [, opts] = (globalThis.fetch as ReturnType<typeof vi.fn>).mock.calls[0];
    expect(opts.body).toBe('{"key":"value"}');
    expect(opts.headers['Content-Type']).toBe('application/json');
  });
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
    // blank line reset currentEventType, so no type override
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
    globalThis.fetch = mockFetchJson({ data: tools });

    const result = await toolApi.list();
    expect(result).toEqual(tools);
    expect(result).toHaveLength(2);
    expect(result[0].name).toBe('sandbox_bash');
  });

  it('list returns empty array when data is null', async () => {
    globalThis.fetch = mockFetchJson({ data: null });

    const result = await toolApi.list();
    expect(result).toEqual([]);
  });

  it('list calls correct endpoint', async () => {
    globalThis.fetch = mockFetchJson({ data: [] });

    await toolApi.list();

    const [path] = (globalThis.fetch as ReturnType<typeof vi.fn>).mock.calls[0];
    expect(path).toBe('/api/tools');
  });
});
