import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { envelopeRouter } from './router';
import { registerChatReducersOnce, resetChatReducersForTest } from './chatReducers';
import { useChatStore } from '../store/chatStore';
import type { Envelope } from './types';

const CONV = 'conv-x';

function envelope<T>(type: string, payload: T): Envelope<T> {
  return {
    seq: 1,
    partition: `chat:${CONV}`,
    type,
    version: 1,
    category: 'business',
    ts: '2026-04-22T00:00:00Z',
    payload,
  } as unknown as Envelope<T>;
}

describe('chatReducers', () => {
  beforeEach(() => {
    resetChatReducersForTest();
    useChatStore.setState({ sessions: {}, streaming: {} });
    registerChatReducersOnce();
  });

  afterEach(() => {
    resetChatReducersForTest();
  });

  it('chat.message.sent appends a user message', () => {
    envelopeRouter.dispatch(
      envelope('chat.message.sent', {
        cardID: 'card-1',
        conversationID: CONV,
        messageID: 'm-1',
        role: 'user',
        content: 'hello',
      }),
    );
    const messages = useChatStore.getState().getSession(CONV).messages;
    expect(messages).toHaveLength(1);
    expect(messages[0]).toMatchObject({ role: 'user', content: 'hello' });
  });

  it('agent.stream.delta appends chunks and finishes', () => {
    envelopeRouter.dispatch(
      envelope('agent.stream.delta', {
        cardID: 'card-1',
        conversationID: CONV,
        runID: 'run-1',
        deltaSeq: 1,
        delta: 'hello',
      }),
    );
    expect(useChatStore.getState().isAgentStreaming(CONV)).toBe(true);
    envelopeRouter.dispatch(
      envelope('agent.stream.delta', {
        cardID: 'card-1',
        conversationID: CONV,
        runID: 'run-1',
        deltaSeq: 2,
        delta: ' world',
        finished: true,
      }),
    );
    expect(useChatStore.getState().isAgentStreaming(CONV)).toBe(false);
    const messages = useChatStore.getState().getSession(CONV).messages;
    expect(messages).toHaveLength(1);
    expect(messages[0].content).toBe('hello world');
  });

  it('chat.callback.dismissed stops streaming', () => {
    useChatStore.getState().startStreaming(CONV);
    useChatStore.getState().appendStreamChunk(CONV, 'partial');
    envelopeRouter.dispatch(
      envelope('chat.callback.dismissed', {
        cardID: 'card-1',
        callbackID: 'cb-1',
        conversationID: CONV,
        reason: 'user_dismissed',
      }),
    );
    expect(useChatStore.getState().isAgentStreaming(CONV)).toBe(false);
  });
});
