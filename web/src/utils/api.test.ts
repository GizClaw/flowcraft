import { describe, it, expect, vi, beforeEach } from 'vitest';
import { apiStream, toolApi, pluginApi, skillApi } from './api';
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

describe('pluginApi', () => {
  it('list returns plugins from /plugins envelope', async () => {
    const plugins = [
      { info: { id: 'a', name: 'A', builtin: true }, status: 'active' as const },
      { info: { id: 'b', name: 'B', builtin: false }, status: 'inactive' as const },
    ];
    const spy = vi.spyOn(client, 'GET').mockResolvedValue({
      data: { data: plugins },
      error: undefined,
      response: new Response(),
    } as never);

    const result = await pluginApi.list();
    expect(spy).toHaveBeenCalledWith('/plugins');
    expect(result).toHaveLength(2);
    expect(result[0].info.id).toBe('a');
  });

  it('list returns empty array when payload is missing', async () => {
    vi.spyOn(client, 'GET').mockResolvedValue({
      data: undefined,
      error: undefined,
      response: new Response(),
    } as never);
    const result = await pluginApi.list();
    expect(result).toEqual([]);
  });

  it('reload normalizes missing added/removed to empty arrays', async () => {
    vi.spyOn(client, 'POST').mockResolvedValue({
      data: {},
      error: undefined,
      response: new Response(),
    } as never);
    const result = await pluginApi.reload();
    expect(result).toEqual({ added: [], removed: [] });
  });

  it('reload preserves real id arrays', async () => {
    vi.spyOn(client, 'POST').mockResolvedValue({
      data: { added: ['x', 'y'], removed: ['z'] },
      error: undefined,
      response: new Response(),
    } as never);
    const result = await pluginApi.reload();
    expect(result).toEqual({ added: ['x', 'y'], removed: ['z'] });
  });

  it('enable calls POST /plugins/{name}/enable with path param', async () => {
    const spy = vi.spyOn(client, 'POST').mockResolvedValue({
      data: { id: 'p1', name: 'P1', builtin: false },
      error: undefined,
      response: new Response(),
    } as never);

    await pluginApi.enable('p1');
    expect(spy).toHaveBeenCalledWith('/plugins/{name}/enable', {
      params: { path: { name: 'p1' } },
    });
  });

  it('disable calls POST /plugins/{name}/disable', async () => {
    const spy = vi.spyOn(client, 'POST').mockResolvedValue({
      data: { id: 'p1', name: 'P1', builtin: false },
      error: undefined,
      response: new Response(),
    } as never);

    await pluginApi.disable('p1');
    expect(spy).toHaveBeenCalledWith('/plugins/{name}/disable', {
      params: { path: { name: 'p1' } },
    });
  });

  it('remove calls DELETE /plugins/{name}', async () => {
    const spy = vi.spyOn(client, 'DELETE').mockResolvedValue({
      data: undefined,
      error: undefined,
      response: new Response(),
    } as never);

    await pluginApi.remove('p1');
    expect(spy).toHaveBeenCalledWith('/plugins/{name}', {
      params: { path: { name: 'p1' } },
    });
  });

  it('upload posts FormData with file field', async () => {
    const spy = vi.spyOn(client, 'POST').mockResolvedValue({
      data: { id: 'echo', name: 'echo', size: 12, added: ['echo'], removed: [] },
      error: undefined,
      response: new Response(),
    } as never);

    const file = new File(['hello-binary'], 'echo', { type: 'application/octet-stream' });
    const result = await pluginApi.upload(file);

    expect(spy).toHaveBeenCalledTimes(1);
    const [path, opts] = spy.mock.calls[0] as [string, { body: unknown }];
    expect(path).toBe('/plugins/upload');
    expect(opts.body).toBeInstanceOf(FormData);
    expect((opts.body as FormData).get('file')).toBeInstanceOf(File);
    expect(result.id).toBe('echo');
    expect(result.added).toEqual(['echo']);
  });

  it('configure calls PUT /plugins/{name}/config with body', async () => {
    const spy = vi.spyOn(client, 'PUT').mockResolvedValue({
      data: { id: 'p1', name: 'P1', builtin: false },
      error: undefined,
      response: new Response(),
    } as never);

    await pluginApi.configure('p1', { temperature: 0.7 });
    expect(spy).toHaveBeenCalledWith('/plugins/{name}/config', {
      params: { path: { name: 'p1' } },
      body: { temperature: 0.7 },
    });
  });
});

describe('skillApi', () => {
  it('list returns skills from /skills envelope', async () => {
    const skills = [
      { name: 'analyzer', builtin: true },
      { name: 'web-search', builtin: false, source: 'https://github.com/foo/bar.git' },
    ];
    const spy = vi.spyOn(client, 'GET').mockResolvedValue({
      data: { data: skills },
      error: undefined,
      response: new Response(),
    } as never);

    const result = await skillApi.list();
    expect(spy).toHaveBeenCalledWith('/skills');
    expect(result).toHaveLength(2);
    expect(result[0].name).toBe('analyzer');
    expect(result[1].source).toBe('https://github.com/foo/bar.git');
  });

  it('list returns empty array when payload is missing', async () => {
    vi.spyOn(client, 'GET').mockResolvedValue({
      data: undefined,
      error: undefined,
      response: new Response(),
    } as never);
    expect(await skillApi.list()).toEqual([]);
  });

  it('install posts /skills/install with the request body', async () => {
    const spy = vi.spyOn(client, 'POST').mockResolvedValue({
      data: { status: 'installed', name: 'web-search', url: 'https://github.com/foo/bar.git' },
      error: undefined,
      response: new Response(),
    } as never);

    const body = { url: 'https://github.com/foo/bar.git', name: 'web-search' };
    await skillApi.install(body);
    expect(spy).toHaveBeenCalledWith('/skills/install', { body });
  });

  it('uninstall calls DELETE /skills/{name}', async () => {
    const spy = vi.spyOn(client, 'DELETE').mockResolvedValue({
      data: undefined,
      error: undefined,
      response: new Response(),
    } as never);

    await skillApi.uninstall('web-search');
    expect(spy).toHaveBeenCalledWith('/skills/{name}', {
      params: { path: { name: 'web-search' } },
    });
  });

  it('update calls PUT /skills/{name} and returns the result', async () => {
    const spy = vi.spyOn(client, 'PUT').mockResolvedValue({
      data: { status: 'updated', name: 'web-search' },
      error: undefined,
      response: new Response(),
    } as never);

    const result = await skillApi.update('web-search');
    expect(spy).toHaveBeenCalledWith('/skills/{name}', {
      params: { path: { name: 'web-search' } },
    });
    expect(result).toEqual({ status: 'updated', name: 'web-search' });
  });

  it('updateAll calls POST /skills/update-all and returns the result', async () => {
    const spy = vi.spyOn(client, 'POST').mockResolvedValue({
      data: { updated: ['a', 'b'] },
      error: undefined,
      response: new Response(),
    } as never);

    const result = await skillApi.updateAll();
    expect(spy).toHaveBeenCalledWith('/skills/update-all', {});
    expect(result).toEqual({ updated: ['a', 'b'] });
  });
});
