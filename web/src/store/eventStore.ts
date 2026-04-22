// eventStore: global connection + envelope telemetry surfaced to the UI.
//
// The store is a thin shadow of the EnvelopeClient state. The client emits
// `connected` / `disconnected` / `envelope` / `heartbeat`; we mirror them
// into observable fields so banners and indicators can react with the
// usual zustand selectors without poking the singleton directly.

import { create } from 'zustand';
import { getEnvelopeClient, type ConnectionState } from '../eventlog/client';
import type { Envelope } from '../eventlog/types';

interface EventState {
  connection: ConnectionState;
  latestSeq: number;
  lastFrameAt: number;
  unreadCount: number;
  subscriptions: string[];

  setConnection: (s: ConnectionState) => void;
  setLatestSeq: (n: number) => void;
  bumpUnread: () => void;
  resetUnread: () => void;
  trackSubscribe: (partition: string) => () => void;
}

export const useEventStore = create<EventState>((set, get) => ({
  connection: 'disconnected',
  latestSeq: 0,
  lastFrameAt: 0,
  unreadCount: 0,
  subscriptions: [],

  setConnection: (s) => set({ connection: s }),
  setLatestSeq: (n) => set({ latestSeq: Math.max(get().latestSeq, n) }),
  bumpUnread: () => set({ unreadCount: get().unreadCount + 1 }),
  resetUnread: () => set({ unreadCount: 0 }),

  trackSubscribe: (partition) => {
    set({ subscriptions: Array.from(new Set([...get().subscriptions, partition])) });
    const dispose = getEnvelopeClient().subscribe(partition, get().latestSeq);
    return () => {
      dispose();
      set({
        subscriptions: get().subscriptions.filter((p) => p !== partition),
      });
    };
  },
}));

let installed = false;

// installEnvelopeWiring connects the singleton client to the store.
// Call once at app boot. Safe to call multiple times.
export function installEnvelopeWiring(): void {
  if (installed) return;
  installed = true;
  const client = getEnvelopeClient();
  client.on('connected', () => useEventStore.getState().setConnection('connected'));
  client.on('disconnected', () => useEventStore.getState().setConnection('disconnected'));
  client.on('heartbeat', (payload: unknown) => {
    const data = payload as { latest_seq?: number };
    if (data?.latest_seq) useEventStore.getState().setLatestSeq(data.latest_seq);
  });
  client.on('envelope', (env: unknown) => {
    const e = env as Envelope;
    useEventStore.getState().setLatestSeq(e.seq);
    useEventStore.getState().bumpUnread();
  });
}

// _resetForTest tears down the wiring so each test starts from a clean slate.
export function _resetForTest(): void {
  installed = false;
  useEventStore.setState({
    connection: 'disconnected',
    latestSeq: 0,
    lastFrameAt: 0,
    unreadCount: 0,
    subscriptions: [],
  });
}
