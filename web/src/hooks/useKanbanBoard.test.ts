import { describe, it, expect, beforeEach, vi } from 'vitest';

vi.mock('../store/authStore', () => ({
  useAuthStore: { getState: () => ({ authenticated: false }) },
}));

import { useChatStore } from '../store/chatStore';
import { handleCallbackMessage, resetCallbackMessageStateForTest } from './useKanbanBoard';

let agentCounter = 0;
let AGENT = 'test-agent-0';

function s() { return useChatStore.getState(); }

function reset() {
  agentCounter++;
  AGENT = `test-agent-${agentCounter}`;
  resetCallbackMessageStateForTest();
  useChatStore.setState({
    sessions: {},
    streaming: {},
  });
  s().ensureSession(AGENT);
}

beforeEach(reset);

describe('handleCallbackMessage', () => {
  it('callback_start begins streaming', () => {
    const ok = handleCallbackMessage(
      { type: 'callback_start', card_id: 'c1' },
      AGENT,
    );
    expect(ok).toBe(true);
    expect(s().isAgentStreaming(AGENT)).toBe(true);
  });

  it('callback_start with query adds user message', () => {
    const cid = `query-${agentCounter}`;
    handleCallbackMessage(
      { type: 'callback_start', card_id: cid, query: '[Task Callback] search completed' },
      AGENT,
    );
    const session = s().getSession(AGENT);
    expect(session.messages).toHaveLength(1);
    expect(session.messages[0].role).toBe('user');
    expect(session.messages[0].content).toBe('[Task Callback] search completed');
    expect(s().isAgentStreaming(AGENT)).toBe(true);
  });

  it('callback_start without query does not add user message', () => {
    const cid = `noquery-${agentCounter}`;
    handleCallbackMessage(
      { type: 'callback_start', card_id: cid },
      AGENT,
    );
    const session = s().getSession(AGENT);
    expect(session.messages).toHaveLength(0);
    expect(s().isAgentStreaming(AGENT)).toBe(true);
  });

  it('callback_token appends chunk', () => {
    handleCallbackMessage(
      { type: 'callback_start', card_id: 'c1' },
      AGENT,
    );
    const ok = handleCallbackMessage(
      { type: 'callback_token', chunk: 'hello', card_id: 'c1', timestamp: '1' },
      AGENT,
    );
    expect(ok).toBe(true);
    expect(s().getStreaming(AGENT).content).toBe('hello');
  });

  it('callback_token ignores missing chunk', () => {
    handleCallbackMessage(
      { type: 'callback_start', card_id: 'c1' },
      AGENT,
    );
    const ok = handleCallbackMessage(
      { type: 'callback_token' },
      AGENT,
    );
    expect(ok).toBe(true);
    expect(s().getStreaming(AGENT).content).toBe('');
  });

  it('callback_tool_call adds tool call', () => {
    handleCallbackMessage(
      { type: 'callback_start', card_id: 'c1' },
      AGENT,
    );
    const ok = handleCallbackMessage(
      {
        type: 'callback_tool_call',
        card_id: 'c1',
        tool_call_id: 'tc1',
        tool_name: 'search',
        tool_args: '{"q":"go"}',
      },
      AGENT,
    );
    expect(ok).toBe(true);
    expect(s().getStreaming(AGENT).toolCalls).toHaveLength(1);
    expect(s().getStreaming(AGENT).toolCalls[0].name).toBe('search');
    expect(s().getStreaming(AGENT).toolCalls[0].args).toBe('{"q":"go"}');
  });

  it('callback_tool_call commits intermediate message if content exists', () => {
    handleCallbackMessage(
      { type: 'callback_start', card_id: 'c2' },
      AGENT,
    );
    s().appendStreamChunk(AGENT, 'partial text');
    handleCallbackMessage(
      {
        type: 'callback_tool_call',
        tool_name: 'calc', card_id: 'c2', tool_call_id: 'tc2',
      },
      AGENT,
    );
    const session = s().getSession(AGENT);
    expect(session.messages.length).toBeGreaterThanOrEqual(1);
    expect(session.messages[session.messages.length - 1].content).toBe('partial text');
  });

  it('callback_tool_result updates tool result', () => {
    handleCallbackMessage(
      { type: 'callback_start', card_id: 'c1' },
      AGENT,
    );
    s().addToolCall(AGENT, { id: 'tc1', name: 'search', args: '{}', status: 'pending' });
    const ok = handleCallbackMessage(
      {
        type: 'callback_tool_result',
        card_id: 'c1',
        tool_call_id: 'tc1',
        tool_name: 'search',
        tool_result: 'result data',
        timestamp: '2',
      },
      AGENT,
    );
    expect(ok).toBe(true);
    const tc = s().getStreaming(AGENT).toolCalls.find((t) => t.name === 'search');
    expect(tc?.result).toBe('result data');
    expect(tc?.status).toBe('success');
  });

  it('callback_tool_result marks error', () => {
    handleCallbackMessage(
      { type: 'callback_start', card_id: 'c1' },
      AGENT,
    );
    s().addToolCall(AGENT, { id: 'tc1', name: 'broken', args: '{}', status: 'pending' });
    handleCallbackMessage(
      {
        type: 'callback_tool_result',
        card_id: 'c1',
        tool_call_id: 'tc1',
        tool_name: 'broken',
        tool_result: 'oops',
        is_error: true,
        timestamp: '3',
      },
      AGENT,
    );
    const tc = s().getStreaming(AGENT).toolCalls.find((t) => t.name === 'broken');
    expect(tc?.status).toBe('error');
  });

  it('callback_done finishes streaming and creates message', () => {
    handleCallbackMessage(
      { type: 'callback_start', card_id: 'c1' },
      AGENT,
    );
    s().appendStreamChunk(AGENT, 'final answer');
    const ok = handleCallbackMessage(
      { type: 'callback_done', card_id: 'c1' },
      AGENT,
    );
    expect(ok).toBe(true);
    expect(s().isAgentStreaming(AGENT)).toBe(false);
    const session = s().getSession(AGENT);
    expect(session.messages).toHaveLength(1);
    expect(session.messages[0].content).toBe('final answer');
  });

  it('callback_done with error appends visible failure message', () => {
    handleCallbackMessage(
      { type: 'callback_start', card_id: 'c-err' },
      AGENT,
    );
    const ok = handleCallbackMessage(
      { type: 'callback_done', card_id: 'c-err', error: 'dispatcher crashed' },
      AGENT,
    );
    expect(ok).toBe(true);
    expect(s().isAgentStreaming(AGENT)).toBe(false);
    const session = s().getSession(AGENT);
    expect(session.messages).toHaveLength(1);
    expect(session.messages[0].content).toBe('Error: dispatcher crashed');
  });

  it('returns false for unknown message type', () => {
    const ok = handleCallbackMessage(
      { type: 'something_else' },
      AGENT,
    );
    expect(ok).toBe(false);
  });

  it('full callback flow: start → tokens → tool_call → tool_result → done', () => {
    const cid = `flow-${agentCounter}`;
    handleCallbackMessage(
      { type: 'callback_start', card_id: cid, agent_id: 'a1' },
      AGENT,
    );
    expect(s().isAgentStreaming(AGENT)).toBe(true);

    handleCallbackMessage(
      { type: 'callback_token', chunk: 'Let me ', card_id: cid, timestamp: 'ft1' },
      AGENT,
    );
    handleCallbackMessage(
      { type: 'callback_token', chunk: 'search.', card_id: cid, timestamp: 'ft2' },
      AGENT,
    );
    expect(s().getStreaming(AGENT).content).toBe('Let me search.');

    handleCallbackMessage(
      {
        type: 'callback_tool_call',
        card_id: cid, tool_call_id: 'ftc1', tool_name: 'web_search', tool_args: '{"q":"test"}',
      },
      AGENT,
    );
    expect(s().getStreaming(AGENT).toolCalls).toHaveLength(1);
    const preSession = s().getSession(AGENT);
    expect(preSession.messages).toHaveLength(1);
    expect(preSession.messages[0].content).toBe('Let me search.');

    handleCallbackMessage(
      {
        type: 'callback_tool_result',
        card_id: cid, tool_call_id: 'ftc1', tool_name: 'web_search', tool_result: 'found 3', timestamp: 'ftr1',
      },
      AGENT,
    );

    handleCallbackMessage(
      { type: 'callback_done', card_id: cid },
      AGENT,
    );

    expect(s().isAgentStreaming(AGENT)).toBe(false);
    const session = s().getSession(AGENT);
    expect(session.messages.length).toBeGreaterThanOrEqual(2);
  });

  it('full callback flow with query: user message + streaming response', () => {
    const cid = `qflow-${agentCounter}`;
    handleCallbackMessage(
      { type: 'callback_start', card_id: cid, agent_id: 'a1', query: '[Task Callback] analysis done' },
      AGENT,
    );
    expect(s().isAgentStreaming(AGENT)).toBe(true);
    const startSession = s().getSession(AGENT);
    expect(startSession.messages).toHaveLength(1);
    expect(startSession.messages[0].role).toBe('user');
    expect(startSession.messages[0].content).toBe('[Task Callback] analysis done');

    handleCallbackMessage(
      { type: 'callback_token', chunk: 'Summary: all good', card_id: cid, timestamp: 'qt1' },
      AGENT,
    );

    handleCallbackMessage(
      { type: 'callback_done', card_id: cid },
      AGENT,
    );

    expect(s().isAgentStreaming(AGENT)).toBe(false);
    const session = s().getSession(AGENT);
    expect(session.messages).toHaveLength(2);
    expect(session.messages[0].role).toBe('user');
    expect(session.messages[1].role).toBe('assistant');
    expect(session.messages[1].content).toBe('Summary: all good');
    expect(session.messages[1].isCallback).toBe(true);
  });

  it('queues callback frames until current assistant stream finishes', async () => {
    vi.useFakeTimers();
    try {
      s().startStreaming(AGENT);
      s().appendStreamChunk(AGENT, 'main reply');

      handleCallbackMessage(
        { type: 'callback_start', card_id: 'queued-1', query: '[Task Callback] queued' },
        AGENT,
      );
      handleCallbackMessage(
        { type: 'callback_token', card_id: 'queued-1', chunk: 'callback reply', timestamp: 'qt1' },
        AGENT,
      );
      handleCallbackMessage(
        { type: 'callback_done', card_id: 'queued-1' },
        AGENT,
      );

      expect(s().getSession(AGENT).messages).toHaveLength(0);

      s().finishStreaming(AGENT);
      await vi.runOnlyPendingTimersAsync();

      const session = s().getSession(AGENT);
      expect(session.messages).toHaveLength(3);
      expect(session.messages[0].content).toBe('main reply');
      expect(session.messages[1].role).toBe('user');
      expect(session.messages[1].content).toBe('[Task Callback] queued');
      expect(session.messages[2].role).toBe('assistant');
      expect(session.messages[2].content).toBe('callback reply');
      expect(session.messages[2].isCallback).toBe(true);
    } finally {
      vi.useRealTimers();
    }
  });
});
