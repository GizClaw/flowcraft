import { useEffect, useCallback } from 'react';
import { useNotificationStore } from '../store/notificationStore';
import { useToastStore } from '../store/toastStore';
import type { WorkflowStreamEvent } from '../types/chat';

export function useNotification() {
  const addNotification = useNotificationStore((s) => s.addNotification);
  const addToast = useToastStore((s) => s.addToast);

  const handleStreamEvent = useCallback((event: WorkflowStreamEvent) => {
    switch (event.type) {
      case 'graph_end':
        addNotification({ type: 'success', title: 'Workflow completed', message: `Run ${event.run_id || ''} finished successfully` });
        addToast('success', 'Workflow completed');
        break;
      case 'node_error':
        addNotification({ type: 'error', title: 'Node error', message: `${event.node_id}: ${event.error || 'Unknown error'}` });
        addToast('error', `Node ${event.node_id} failed`);
        break;
      case 'kanban_update': {
        const eventType = event.event_type;
        const payload = event.payload;
        if (eventType === 'kanban.task.completed') {
          addNotification({ type: 'info', title: 'Task completed', message: payload?.card_id || 'A kanban task was completed' });
          addToast('info', 'Kanban task completed');
        } else if (eventType === 'kanban.task.failed') {
          addNotification({ type: 'error', title: 'Task failed', message: payload?.error || payload?.card_id || 'A kanban task failed' });
          addToast('error', 'Kanban task failed');
        }
        break;
      }
    }
  }, [addNotification, addToast]);

  return { handleStreamEvent };
}

export function useNotificationSSE(url: string | null) {
  const { handleStreamEvent } = useNotification();

  useEffect(() => {
    if (!url) return;
    const es = new EventSource(url);
    es.onmessage = (e) => {
      try { handleStreamEvent(JSON.parse(e.data)); } catch { /* skip */ }
    };
    return () => es.close();
  }, [url, handleStreamEvent]);
}
