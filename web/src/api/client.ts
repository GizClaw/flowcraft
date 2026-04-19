import createClient, { type Middleware } from 'openapi-fetch';
import type { paths } from './schema';
import { useToastStore } from '../store/toastStore';

export class ApiError extends Error {
  status: number;
  constructor(status: number, message: string) {
    super(message);
    this.name = 'ApiError';
    this.status = status;
  }
}

const throwOnError: Middleware = {
  async onResponse({ response }) {
    const warning = response.headers.get('X-Warning');
    if (warning) {
      useToastStore.getState().addToast('warning', warning);
    }
    if (!response.ok) {
      const body = await response.clone().json().catch(() => ({}));
      const err = body as { error?: { message?: string }; message?: string };
      throw new ApiError(
        response.status,
        err.error?.message || err.message || response.statusText,
      );
    }
    return undefined;
  },
};

const baseUrl = typeof window !== 'undefined'
  ? `${window.location.origin}/api`
  : 'http://localhost/api';

const client = createClient<paths>({
  baseUrl,
  credentials: 'include',
});

client.use(throwOnError);

export default client;

export async function* apiStream<T = unknown>(
  path: string,
  opts: RequestInit & { json?: unknown; signal?: AbortSignal } = {},
): AsyncGenerator<T> {
  const { json, headers: extra, signal, ...rest } = opts;
  const headers: Record<string, string> = { ...(extra as Record<string, string>) };

  if (json !== undefined) {
    headers['Content-Type'] = 'application/json';
    rest.body = JSON.stringify(json);
  }

  const res = await fetch(path, { headers, signal, credentials: 'include', ...rest });

  if (!res.ok) {
    const body = await res.json().catch(() => ({}));
    const err = body as { error?: { message?: string }; message?: string };
    throw new ApiError(res.status, err.error?.message || err.message || res.statusText);
  }

  const reader = res.body?.getReader();
  if (!reader) return;

  const decoder = new TextDecoder();
  let buffer = '';
  let currentEventType = '';

  try {
    while (true) {
      const { done, value } = await reader.read();
      if (done) break;

      buffer += decoder.decode(value, { stream: true });
      const lines = buffer.split('\n');
      buffer = lines.pop() || '';

      for (const line of lines) {
        const trimmed = line.trim();
        if (trimmed.startsWith('event: ')) {
          currentEventType = trimmed.slice(7).trim();
        } else if (trimmed.startsWith('data: ')) {
          try {
            const parsed = JSON.parse(trimmed.slice(6));
            if (currentEventType && typeof parsed === 'object' && parsed !== null) {
              (parsed as Record<string, unknown>).type = currentEventType;
            }
            yield parsed as T;
          } catch { /* skip malformed */ }
          currentEventType = '';
        } else if (trimmed === '') {
          currentEventType = '';
        }
      }
    }
  } finally {
    reader.releaseLock();
  }
}
