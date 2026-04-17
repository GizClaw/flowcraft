import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { useChatStore } from '../store/chatStore';
import type { WorkflowStreamEvent, ChatRequest } from '../types/chat';

vi.mock('react-i18next', () => ({
  useTranslation: () => ({ t: (k: string) => k }),
}));

vi.mock('./useNotification', () => ({
  useNotification: () => ({ handleStreamEvent: vi.fn() }),
}));

function resetStore() {
  useChatStore.setState({
    sessions: {},
    streaming: {},
  });
}

function s() { return useChatStore.getState(); }

async function* fakeStream(events: WorkflowStreamEvent[]): AsyncGenerator<WorkflowStreamEvent> {
  for (const e of events) yield e;
}

async function simulateSendMessage(
  agentId: string,
  content: string,
  events: WorkflowStreamEvent[],
  opts?: {
    onToolResult?: (e: WorkflowStreamEvent) => void;
    onDone?: (e: WorkflowStreamEvent) => void;
    buildRequest?: (c: string) => Partial<ChatRequest>;
  },
) {
  s().ensureSession(agentId);
  s().addUserMessage(agentId, content);
  const controller = s().startStreaming(agentId);

  let isCallback = false;
  let callbackCardId = '';

  try {
    const stream = fakeStream(events);
    for await (const event of stream) {
      if (controller.signal.aborted) break;

      switch (event.type) {
        case 'agent_token':
          if (event.chunk) {
            const st = s().getStreaming(agentId);
            if (st.toolCalls.length > 0 && st.toolCalls.every((tc) => tc.status !== 'pending')) {
              s().commitIntermediateMessage(agentId);
            }
            s().appendStreamChunk(agentId, event.chunk);
          }
          break;
        case 'agent_tool_call':
          if (event.tool_name) {
            if (s().getStreaming(agentId).content) s().commitIntermediateMessage(agentId);
            s().addToolCall(agentId, { id: event.tool_call_id, name: event.tool_name, args: event.tool_args || '', status: 'pending' });
          }
          break;
        case 'agent_tool_result':
          if (event.tool_name) {
            s().updateToolCallResult(agentId, event.tool_call_id, event.tool_name, event.tool_result || '', event.is_error ? 'error' : 'success');
            opts?.onToolResult?.(event);
          }
          break;
        case 'done': {
          const output = event.output;
          if (output?.metadata?.callback) {
            isCallback = true;
            callbackCardId = (output.metadata.card_id as string) || '';
          }
          opts?.onDone?.(event);
          break;
        }
        case 'error':
          s().appendStreamChunk(agentId, `Error: ${event.error || event.message || 'Unknown error'}`);
          break;
      }
    }
  } finally {
    s().finishStreaming(agentId, isCallback ? { isCallback: true, cardId: callbackCardId || undefined } : undefined);
  }
}

beforeEach(resetStore);
afterEach(() => vi.restoreAllMocks());

// ── Basic flow ──

describe('basic send/receive flow', () => {
  it('sends user message and receives streamed assistant response', async () => {
    await simulateSendMessage('a1', 'hi', [
      { type: 'agent_token', chunk: 'hello ' },
      { type: 'agent_token', chunk: 'world' },
      { type: 'done', output: { conversation_id: 'c1', message_id: 'm1', answer: '', elapsed_ms: 100 }, conversation_id: 'c1' },
    ]);

    const ses = s().getSession('a1');
    expect(ses.messages).toHaveLength(2);
    expect(ses.messages[0].role).toBe('user');
    expect(ses.messages[0].content).toBe('hi');
    expect(ses.messages[1].role).toBe('assistant');
    expect(ses.messages[1].content).toBe('hello world');
    expect(s().isAgentStreaming('a1')).toBe(false);
  });

  it('does not create assistant message for empty stream', async () => {
    await simulateSendMessage('a1', 'hi', [
      { type: 'done', output: { conversation_id: 'c1', message_id: '', answer: '', elapsed_ms: 0 } },
    ]);

    expect(s().getSession('a1').messages).toHaveLength(1);
    expect(s().getSession('a1').messages[0].role).toBe('user');
  });
});

// ── Tool call flow ──

describe('tool call flow', () => {
  it('processes tool_call → tool_result → text', async () => {
    const onToolResult = vi.fn();

    await simulateSendMessage('a1', 'search something', [
      { type: 'agent_tool_call', tool_call_id: 'tc1', tool_name: 'web_search', tool_args: '{"q":"test"}' },
      { type: 'agent_tool_result', tool_call_id: 'tc1', tool_name: 'web_search', tool_result: '{"results":[]}' },
      { type: 'agent_token', chunk: 'Based on results...' },
      { type: 'done', output: { conversation_id: 'c1', message_id: '', answer: '', elapsed_ms: 0 } },
    ], { onToolResult });

    const msgs = s().getSession('a1').messages;
    expect(msgs).toHaveLength(3);
    expect(msgs[0].role).toBe('user');
    expect(msgs[1].toolCalls).toHaveLength(1);
    expect(msgs[1].toolCalls![0].name).toBe('web_search');
    expect(msgs[1].toolCalls![0].status).toBe('success');
    expect(msgs[2].content).toBe('Based on results...');
    expect(onToolResult).toHaveBeenCalledTimes(1);
  });

  it('processes multiple tool calls in sequence', async () => {
    await simulateSendMessage('a1', 'multi-tool', [
      { type: 'agent_tool_call', tool_call_id: 'tc1', tool_name: 'tool_a', tool_args: '{}' },
      { type: 'agent_tool_result', tool_call_id: 'tc1', tool_name: 'tool_a', tool_result: 'a-result' },
      { type: 'agent_tool_call', tool_call_id: 'tc2', tool_name: 'tool_b', tool_args: '{}' },
      { type: 'agent_tool_result', tool_call_id: 'tc2', tool_name: 'tool_b', tool_result: 'b-result' },
      { type: 'agent_token', chunk: 'done' },
      { type: 'done', output: { conversation_id: 'c1', message_id: '', answer: '', elapsed_ms: 0 } },
    ]);

    const msgs = s().getSession('a1').messages;
    expect(msgs.length).toBeGreaterThanOrEqual(3);
    const toolMsgs = msgs.filter((m) => m.toolCalls?.length);
    const allToolNames = toolMsgs.flatMap((m) => m.toolCalls!.map((tc) => tc.name));
    expect(allToolNames).toContain('tool_a');
    expect(allToolNames).toContain('tool_b');
    const lastMsg = msgs.at(-1)!;
    expect(lastMsg.content).toBe('done');
  });

  it('handles tool result error', async () => {
    await simulateSendMessage('a1', 'fail tool', [
      { type: 'agent_tool_call', tool_call_id: 'tc1', tool_name: 'bad_tool', tool_args: '{}' },
      { type: 'agent_tool_result', tool_call_id: 'tc1', tool_name: 'bad_tool', tool_result: 'error msg', is_error: true },
      { type: 'agent_token', chunk: 'recovered' },
      { type: 'done', output: { conversation_id: 'c1', message_id: '', answer: '', elapsed_ms: 0 } },
    ]);

    const msgs = s().getSession('a1').messages;
    const toolMsg = msgs.find((m) => m.toolCalls?.length);
    expect(toolMsg!.toolCalls![0].status).toBe('error');
    expect(toolMsg!.toolCalls![0].result).toBe('error msg');
  });

  it('text before tool call creates intermediate commit', async () => {
    await simulateSendMessage('a1', 'think then act', [
      { type: 'agent_token', chunk: 'Let me think...' },
      { type: 'agent_tool_call', tool_call_id: 'tc1', tool_name: 'search', tool_args: '{}' },
      { type: 'agent_tool_result', tool_call_id: 'tc1', tool_name: 'search', tool_result: 'found' },
      { type: 'agent_token', chunk: 'Here is the result' },
      { type: 'done', output: { conversation_id: 'c1', message_id: '', answer: '', elapsed_ms: 0 } },
    ]);

    const msgs = s().getSession('a1').messages;
    expect(msgs.length).toBeGreaterThanOrEqual(3);
    const textMsgs = msgs.filter((m) => m.role === 'assistant' && m.content && !m.toolCalls?.length);
    expect(textMsgs.length).toBeGreaterThanOrEqual(2);
    expect(textMsgs[0].content).toBe('Let me think...');
  });
});

// ── Done event ──

describe('done event', () => {
  it('marks callback when metadata.callback is truthy', async () => {
    await simulateSendMessage('a1', 'callback trigger', [
      { type: 'agent_token', chunk: 'callback result' },
      { type: 'done', output: { conversation_id: '', message_id: '', answer: '', elapsed_ms: 0, metadata: { callback: true, card_id: 'card-abc' } } },
    ]);

    const lastMsg = s().getSession('a1').messages.at(-1)!;
    expect(lastMsg.isCallback).toBe(true);
    expect(lastMsg.cardId).toBe('card-abc');
  });

  it('calls onDone callback', async () => {
    const onDone = vi.fn();
    await simulateSendMessage('a1', 'hello', [
      { type: 'done', output: { conversation_id: '', message_id: '', answer: '', elapsed_ms: 0 } },
    ], { onDone });

    expect(onDone).toHaveBeenCalledTimes(1);
  });
});

describe('error event', () => {
  it('writes stream error into assistant message', async () => {
    await simulateSendMessage('a1', 'please fail', [
      { type: 'error', message: 'backend exploded' },
    ]);

    const msgs = s().getSession('a1').messages;
    expect(msgs).toHaveLength(2);
    expect(msgs[0].role).toBe('user');
    expect(msgs[1].role).toBe('assistant');
    expect(msgs[1].content).toBe('Error: backend exploded');
  });
});

// ── Abort ──

describe('abort/stop', () => {
  it('stopStreaming aborts ongoing stream', () => {
    s().ensureSession('a1');
    const ctrl = s().startStreaming('a1');
    s().stopStreaming('a1');
    expect(ctrl.signal.aborted).toBe(true);
  });
});

// ── Edge cases ──

describe('edge cases', () => {
  it('agent_token with empty chunk is ignored', async () => {
    await simulateSendMessage('a1', 'hi', [
      { type: 'agent_token', chunk: '' },
      { type: 'agent_token', chunk: undefined as unknown as string },
      { type: 'agent_token', chunk: 'real' },
      { type: 'done', output: { conversation_id: '', message_id: '', answer: '', elapsed_ms: 0 } },
    ]);

    const msgs = s().getSession('a1').messages;
    const assistantMsgs = msgs.filter((m) => m.role === 'assistant');
    expect(assistantMsgs).toHaveLength(1);
    expect(assistantMsgs[0].content).toBe('real');
  });

  it('tool_call without tool_name is ignored', async () => {
    await simulateSendMessage('a1', 'hi', [
      { type: 'agent_tool_call' } as WorkflowStreamEvent,
      { type: 'agent_token', chunk: 'ok' },
      { type: 'done', output: { conversation_id: '', message_id: '', answer: '', elapsed_ms: 0 } },
    ]);

    const msgs = s().getSession('a1').messages;
    const withTools = msgs.filter((m) => m.toolCalls?.length);
    expect(withTools).toHaveLength(0);
  });

  it('tool_result without tool_name is ignored', async () => {
    await simulateSendMessage('a1', 'hi', [
      { type: 'agent_tool_result' } as WorkflowStreamEvent,
      { type: 'agent_token', chunk: 'ok' },
      { type: 'done', output: { conversation_id: '', message_id: '', answer: '', elapsed_ms: 0 } },
    ]);

    const msgs = s().getSession('a1').messages;
    expect(msgs.filter((m) => m.role === 'assistant')).toHaveLength(1);
  });
});
