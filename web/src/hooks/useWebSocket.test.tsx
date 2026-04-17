import { useEffect } from 'react';
import { render, waitFor } from '@testing-library/react';
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { useWebSocket } from './useWebSocket';

vi.mock('../store/authStore', () => ({
  useAuthStore: {
    getState: () => ({ authenticated: false }),
  },
}));

class MockWebSocket {
  static instances: MockWebSocket[] = [];
  static OPEN = 1;
  readyState = MockWebSocket.OPEN;
  url: string;
  onopen: (() => void) | null = null;
  onmessage: ((event: MessageEvent<string>) => void) | null = null;
  onclose: (() => void) | null = null;
  onerror: (() => void) | null = null;

  constructor(url: string) {
    this.url = url;
    MockWebSocket.instances.push(this);
    queueMicrotask(() => this.onopen?.());
  }

  send() {}

  close() {
    this.onclose?.();
  }
}

function TestComponent({ url }: { url: string | null }) {
  const { close } = useWebSocket(url, {
    onMessage: () => {},
    reconnectInterval: 0,
  });

  useEffect(() => () => close(), [close]);
  return null;
}

beforeEach(() => {
  MockWebSocket.instances = [];
  vi.restoreAllMocks();
  globalThis.WebSocket = MockWebSocket as unknown as typeof WebSocket;
});

afterEach(() => {
  vi.restoreAllMocks();
});

describe('useWebSocket', () => {
  it('requests websocket ticket before connecting to /api/ws', async () => {
    globalThis.fetch = vi.fn().mockResolvedValue({
      ok: true,
      status: 200,
      headers: new Headers(),
      json: () => Promise.resolve({
        ticket: 'ticket-123',
        expires_at: new Date().toISOString(),
      }),
    }) as typeof fetch;

    render(<TestComponent url="/api/ws" />);

    await waitFor(() => {
      expect(globalThis.fetch).toHaveBeenCalledWith('/api/ws-ticket', expect.objectContaining({ method: 'POST' }));
      expect(MockWebSocket.instances).toHaveLength(1);
      expect(MockWebSocket.instances[0].url).toContain('/api/ws?ticket=ticket-123');
    });
  });
});
