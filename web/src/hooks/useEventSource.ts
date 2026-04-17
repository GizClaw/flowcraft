import { useEffect, useRef, useCallback } from 'react';

export function useEventSource(url: string | null, onMessage: (data: unknown) => void) {
  const esRef = useRef<EventSource | null>(null);

  const cleanup = useCallback(() => {
    if (esRef.current) {
      esRef.current.close();
      esRef.current = null;
    }
  }, []);

  useEffect(() => {
    if (!url) return;

    const es = new EventSource(url);
    esRef.current = es;

    es.onmessage = (event) => {
      try {
        const data = JSON.parse(event.data);
        onMessage(data);
      } catch { /* skip */ }
    };

    es.onerror = () => {
      es.close();
    };

    return cleanup;
  }, [url, onMessage, cleanup]);

  return { close: cleanup };
}
