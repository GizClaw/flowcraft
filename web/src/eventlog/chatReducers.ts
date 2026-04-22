// Chat envelope reducers.
//
// The R4 contract is: every chat-relevant frame from the backend is an
// envelope; `chatStore` is updated *only* through these reducers. The
// previous `pendingCallbackMessages` global map (a CoPilot-era WS hack
// that buffered callback_* frames while another agent was streaming) is
// no longer needed: each envelope carries its own conversation_id and
// is dispatched independently, so order is preserved by `seq` rather
// than by an in-process queue.
//
// Reducer responsibilities:
//   * chat.message.sent          — append user-authored message
//   * chat.callback.queued       — start (or extend) an assistant stream
//   * chat.callback.delivered    — finish the stream
//   * chat.callback.dismissed    — drop the in-flight stream silently
//   * agent.stream.delta         — append a chunk to the active stream
//
// The reducers key everything by `conversationID` (which the chat
// projector also uses as its primary key). The frontend used to key by
// `chatAgentId`; conversation_id is always present in the envelope and
// removes the ambiguity when one agent has multiple parallel chats.
import type { Envelope } from './types';
import type {
  AgentStreamDeltaPayload,
  ChatCallbackDeliveredPayload,
  ChatCallbackDismissedPayload,
  ChatCallbackQueuedPayload,
  ChatMessageSentPayload,
} from '../api/event_payloads.gen';
import { useChatStore } from '../store/chatStore';
import { envelopeRouter } from './router';

let registered = false;
const disposers: Array<() => void> = [];

export function registerChatReducersOnce(): void {
  if (registered) return;
  registered = true;

  disposers.push(
    envelopeRouter.on<ChatMessageSentPayload>('chat.message.sent', (env) => {
      const p = env.payload as ChatMessageSentPayload;
      const convID = p.conversationID;
      if (!convID) return;
      if ((p.role ?? '').toLowerCase() === 'user' && p.content) {
        useChatStore.getState().addUserMessage(convID, p.content);
      }
    }),
  );

  disposers.push(
    envelopeRouter.on<ChatCallbackQueuedPayload>('chat.callback.queued', (env) => {
      const p = env.payload as ChatCallbackQueuedPayload;
      const convID = p.conversationID;
      if (!convID) return;
      const store = useChatStore.getState();
      store.ensureSession(convID);
      if (!store.isAgentStreaming(convID)) {
        store.startStreaming(convID);
      }
      if (p.content && p.contentType !== 'status_update') {
        store.appendStreamChunk(convID, p.content);
      }
    }),
  );

  disposers.push(
    envelopeRouter.on<AgentStreamDeltaPayload>('agent.stream.delta', (env) => {
      const p = env.payload as AgentStreamDeltaPayload;
      const convID = p.conversationID;
      if (!convID) return;
      const store = useChatStore.getState();
      store.ensureSession(convID);
      if (!store.isAgentStreaming(convID)) {
        store.startStreaming(convID);
      }
      if (p.delta) {
        store.appendStreamChunk(convID, p.delta);
      }
      if (p.finished) {
        store.finishStreaming(convID, { isCallback: true, cardId: p.cardID });
      }
    }),
  );

  disposers.push(
    envelopeRouter.on<ChatCallbackDeliveredPayload>('chat.callback.delivered', (env) => {
      const p = env.payload as ChatCallbackDeliveredPayload;
      const convID = p.conversationID;
      if (!convID) return;
      const store = useChatStore.getState();
      if (store.isAgentStreaming(convID)) {
        store.finishStreaming(convID, { isCallback: true, cardId: p.cardID });
      }
    }),
  );

  disposers.push(
    envelopeRouter.on<ChatCallbackDismissedPayload>('chat.callback.dismissed', (env) => {
      const p = env.payload as ChatCallbackDismissedPayload;
      const convID = p.conversationID;
      if (!convID) return;
      const store = useChatStore.getState();
      if (store.isAgentStreaming(convID)) {
        store.stopStreaming(convID);
        store.finishStreaming(convID, { isCallback: true, cardId: p.cardID });
      }
    }),
  );
}

// Test helper. Allows resetting the registration so each unit test can
// install its own reducers without leaking state across files.
export function resetChatReducersForTest(): void {
  while (disposers.length > 0) {
    const dispose = disposers.pop();
    try {
      dispose?.();
    } catch (err) {
      console.error('chatReducers: dispose threw', err);
    }
  }
  registered = false;
}

// Re-exported only so a single caller (`Envelope`) doesn't need to import
// from two different places.
export type { Envelope };
