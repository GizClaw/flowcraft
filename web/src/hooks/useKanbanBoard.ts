import { useEffect, useRef } from 'react';
import { useKanbanStore } from '../store/kanbanStore';
import { useCoPilotStore } from '../store/copilotStore';
import { useEventStore } from '../store/eventStore';
import { kanbanApi } from '../utils/api';
import type { Envelope } from '../eventlog/types';
import { envelopeRouter } from '../eventlog/router';
import { registerChatReducersOnce } from '../eventlog/chatReducers';
import { mapCardStatusToUI } from '../types/kanban';

let kanbanReducersRegistered = false;
function registerKanbanReducersOnce() {
  if (kanbanReducersRegistered) return;
  kanbanReducersRegistered = true;
  envelopeRouter.on('task.submitted', (e) => {
    useKanbanStore.getState().applyTaskSubmitted(e as Envelope<{ card_id: string; runtime_id: string; target_agent_id?: string; query?: string }>);
    useCoPilotStore.getState().updateDispatchedTaskStatus((e.payload as { card_id: string }).card_id, mapCardStatusToUI('pending'));
  });
  envelopeRouter.on('task.claimed', (e) => {
    useKanbanStore.getState().applyTaskClaimed(e as Envelope<{ card_id: string; runtime_id: string; target_agent_id?: string }>);
    useCoPilotStore.getState().updateDispatchedTaskStatus((e.payload as { card_id: string }).card_id, mapCardStatusToUI('claimed'));
  });
  envelopeRouter.on('task.completed', (e) => {
    useKanbanStore.getState().applyTaskCompleted(e as Envelope<{ card_id: string; runtime_id: string; target_agent_id?: string; result?: string; elapsed_ms?: number }>);
    useCoPilotStore.getState().updateDispatchedTaskStatus((e.payload as { card_id: string }).card_id, mapCardStatusToUI('done'));
    maybeStopBackground();
  });
  envelopeRouter.on('task.failed', (e) => {
    useKanbanStore.getState().applyTaskFailed(e as Envelope<{ card_id: string; runtime_id: string; target_agent_id?: string; error?: string; elapsed_ms?: number }>);
    useCoPilotStore.getState().updateDispatchedTaskStatus((e.payload as { card_id: string }).card_id, mapCardStatusToUI('failed'));
    maybeStopBackground();
  });
}

function maybeStopBackground() {
  const store = useKanbanStore.getState();
  const hasPending = store.getCardsByStatus('pending').length > 0 || store.getCardsByStatus('claimed').length > 0;
  if (!hasPending) {
    useCoPilotStore.getState().setBackgroundRunning(false);
  }
}

// fullResync grabs the current /kanban/cards snapshot once at boot or on
// runtime switch. Per §7.1.5 the steady-state KanbanStore is fed by
// envelopes only; fullResync covers events from before EnvelopeClient
// connected. The response carries `last_seq`; we hand it to the store so
// the next envelope frame is reconciled against the snapshot cursor
// (drops anything ≤ last_seq, which would otherwise double-apply).
function fullResync() {
  kanbanApi.cards()
    .then((snap) => useKanbanStore.getState().loadSnapshot(snap.cards, snap.lastSeq))
    .catch((err) => console.error('kanban: snapshot load failed:', err));
}

// useKanbanBoard wires the kanban view to the unified envelope stream.
// chat.callback.* envelopes are projected into chatStore by
// chatReducers.ts, so panel-side delegation is gone — callers don't
// need to pass an agent identifier anymore.
export function useKanbanBoard(runtimeId: string | null) {
  const setRuntimeId = useKanbanStore((s) => s.setRuntimeId);
  const reset = useKanbanStore((s) => s.reset);
  const prevRuntimeRef = useRef<string | null>(null);

  useEffect(() => {
    registerKanbanReducersOnce();
    registerChatReducersOnce();
  }, []);

  useEffect(() => {
    if (!runtimeId) {
      if (prevRuntimeRef.current !== null) reset();
      prevRuntimeRef.current = null;
      return;
    }
    setRuntimeId(runtimeId);
    fullResync();
    prevRuntimeRef.current = runtimeId;
  }, [runtimeId, setRuntimeId, reset]);

  // §13 / Track-A: subscribe to the runtime envelope partition. The
  // EnvelopeClient delivers task.* / agent.config.changed / agent.stream.*
  // / cron.* envelopes through envelopeRouter (registered above).
  // EnvelopeClient owns its own resume/reconnect loop and replays missed
  // seqs via `since`, so no manual onOpen resync hook is needed.
  useEffect(() => {
    if (!runtimeId) return;
    return useEventStore.getState().trackSubscribe(`runtime:${runtimeId}`);
  }, [runtimeId]);
}
