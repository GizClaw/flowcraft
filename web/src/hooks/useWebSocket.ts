import { useEffect, useRef, useCallback } from 'react';
import { wsApi } from '../utils/api';

interface WSOptions {
  onMessage: (data: unknown) => void;
  onOpen?: () => void;
  onClose?: () => void;
  reconnectInterval?: number;
}

interface SharedConnection {
  ws: WebSocket | null;
  listeners: Set<WSOptions>;
  reconnectTimer?: ReturnType<typeof setTimeout>;
  refCount: number;
}

const connections = new Map<string, SharedConnection>();

function resolveUrl(url: string): string {
  if (url.startsWith('/')) {
    const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
    return `${proto}//${location.host}${url}`;
  }
  return url;
}

async function resolveConnectUrl(url: string): Promise<string> {
  if (!url.startsWith('/api/ws')) return url;
  const ticket = await wsApi.ticket();
  const sep = url.includes('?') ? '&' : '?';
  return `${url}${sep}ticket=${encodeURIComponent(ticket.ticket)}`;
}

function getOrCreateConnection(key: string, reconnectInterval?: number): SharedConnection {
  const existing = connections.get(key);
  if (existing) {
    existing.refCount++;
    return existing;
  }

  const conn: SharedConnection = {
    ws: null,
    listeners: new Set(),
    refCount: 1,
  };
  connections.set(key, conn);

  async function connect() {
    try {
      const connectUrl = await resolveConnectUrl(key);
      if (conn.refCount <= 0) return;
      const ws = new WebSocket(resolveUrl(connectUrl));
      conn.ws = ws;

      ws.onopen = () => {
        conn.listeners.forEach((l) => l.onOpen?.());
      };

      ws.onmessage = (event) => {
        try {
          const data = JSON.parse(event.data);
          conn.listeners.forEach((l) => l.onMessage(data));
        } catch { /* skip */ }
      };

      ws.onclose = () => {
        conn.listeners.forEach((l) => l.onClose?.());
        if (conn.refCount > 0 && reconnectInterval) {
          conn.reconnectTimer = setTimeout(connect, reconnectInterval);
        }
      };

      ws.onerror = () => ws.close();
    } catch {
      conn.listeners.forEach((l) => l.onClose?.());
      if (conn.refCount > 0 && reconnectInterval) {
        conn.reconnectTimer = setTimeout(connect, reconnectInterval);
      }
    }
  }

  connect();
  return conn;
}

function releaseConnection(key: string, conn: SharedConnection) {
  conn.refCount--;
  if (conn.refCount <= 0) {
    clearTimeout(conn.reconnectTimer);
    conn.ws?.close();
    connections.delete(key);
  }
}

export function useWebSocket(url: string | null, options: WSOptions) {
  const optionsRef = useRef(options);
  optionsRef.current = options;

  const stableListener = useRef<WSOptions>({
    onMessage: (data) => optionsRef.current.onMessage(data),
    onOpen: () => optionsRef.current.onOpen?.(),
    onClose: () => optionsRef.current.onClose?.(),
  });

  useEffect(() => {
    if (!url) return;

    const conn = getOrCreateConnection(url, optionsRef.current.reconnectInterval);
    const listener = stableListener.current;
    conn.listeners.add(listener);

    if (conn.ws?.readyState === WebSocket.OPEN) {
      listener.onOpen?.();
    }

    return () => {
      conn.listeners.delete(listener);
      releaseConnection(url, conn);
    };
  }, [url]);

  const send = useCallback((data: unknown) => {
    if (!url) return;
    const conn = connections.get(url);
    if (conn?.ws?.readyState === WebSocket.OPEN) {
      conn.ws.send(JSON.stringify(data));
    }
  }, [url]);

  const close = useCallback(() => {
    if (!url) return;
    const conn = connections.get(url);
    conn?.ws?.close();
  }, [url]);

  return { send, close };
}
