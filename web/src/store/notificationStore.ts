import { create } from 'zustand';

export interface Notification {
  id: string;
  type: 'success' | 'error' | 'info';
  title: string;
  message: string;
  read: boolean;
  timestamp: string;
}

interface NotificationState {
  notifications: Notification[];
  unreadCount: number;
  addNotification: (n: Omit<Notification, 'id' | 'read' | 'timestamp'>) => void;
  markRead: (id: string) => void;
  markAllRead: () => void;
  clear: () => void;
}

function loadFromStorage(): Notification[] {
  try {
    const raw = localStorage.getItem('flowcraft_notifications');
    return raw ? JSON.parse(raw) : [];
  } catch {
    return [];
  }
}

function saveToStorage(notifications: Notification[]) {
  localStorage.setItem('flowcraft_notifications', JSON.stringify(notifications.slice(0, 100)));
}

let counter = 0;

export const useNotificationStore = create<NotificationState>((set, get) => {
  const initial = loadFromStorage();
  return {
    notifications: initial,
    unreadCount: initial.filter((n) => !n.read).length,

    addNotification: (n) => {
      const notification: Notification = {
        ...n,
        id: `notif-${++counter}-${Date.now()}`,
        read: false,
        timestamp: new Date().toISOString(),
      };
      const next = [notification, ...get().notifications].slice(0, 100);
      saveToStorage(next);
      set({ notifications: next, unreadCount: next.filter((x) => !x.read).length });
    },

    markRead: (id) => {
      const next = get().notifications.map((n) => (n.id === id ? { ...n, read: true } : n));
      saveToStorage(next);
      set({ notifications: next, unreadCount: next.filter((x) => !x.read).length });
    },

    markAllRead: () => {
      const next = get().notifications.map((n) => ({ ...n, read: true }));
      saveToStorage(next);
      set({ notifications: next, unreadCount: 0 });
    },

    clear: () => {
      localStorage.removeItem('flowcraft_notifications');
      set({ notifications: [], unreadCount: 0 });
    },
  };
});
