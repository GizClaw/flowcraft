import { describe, it, expect, beforeEach } from 'vitest';
import { useChatStore } from './chatStore';
import type { RichMessage } from '../types/chat';

function s() { return useChatStore.getState(); }

function reset() {
  useChatStore.setState({
    sessions: {},
    streaming: {},
  });
}

beforeEach(reset);

// ── Session management ──

describe('session management', () => {
  it('ensureSession creates empty session', () => {
    s().ensureSession('a1');
    const ses = s().getSession('a1');
    expect(ses.messages).toEqual([]);
    expect(ses.historyLoaded).toBe(false);
  });

  it('ensureSession is idempotent', () => {
    s().ensureSession('a1');
    s().addUserMessage('a1', 'hi');
    s().ensureSession('a1');
    expect(s().getSession('a1').messages).toHaveLength(1);
  });

  it('getSession returns empty for unknown agent', () => {
    const ses = s().getSession('unknown');
    expect(ses.messages).toEqual([]);
    expect(ses.historyLoaded).toBe(false);
  });

  it('clearSession removes the session entirely', () => {
    s().ensureSession('a1');
    s().addUserMessage('a1', 'data');
    s().clearSession('a1');
    expect(s().getSession('a1').messages).toEqual([]);
    expect(s().sessions['a1']).toBeUndefined();
  });
});

// ── Multi-agent isolation ──

describe('multi-agent isolation', () => {
  it('messages are isolated between agents', () => {
    s().ensureSession('a1');
    s().ensureSession('a2');
    s().addUserMessage('a1', 'hello a1');
    s().addUserMessage('a2', 'hello a2');

    expect(s().getSession('a1').messages).toHaveLength(1);
    expect(s().getSession('a1').messages[0].content).toBe('hello a1');
    expect(s().getSession('a2').messages).toHaveLength(1);
    expect(s().getSession('a2').messages[0].content).toBe('hello a2');
  });

  it('streaming targets specific agent', () => {
    s().ensureSession('a1');
    s().ensureSession('a2');
    s().addUserMessage('a2', 'pre-existing');

    s().startStreaming('a1');
    s().appendStreamChunk('a1', 'token');
    s().finishStreaming('a1');

    expect(s().getSession('a1').messages).toHaveLength(1);
    expect(s().getSession('a1').messages[0].content).toBe('token');
    expect(s().getSession('a2').messages).toHaveLength(1);
    expect(s().getSession('a2').messages[0].content).toBe('pre-existing');
  });

  it('two agents can stream concurrently', () => {
    s().ensureSession('a1');
    s().ensureSession('a2');

    s().startStreaming('a1');
    s().startStreaming('a2');
    s().appendStreamChunk('a1', 'from-a1');
    s().appendStreamChunk('a2', 'from-a2');

    expect(s().getStreaming('a1').content).toBe('from-a1');
    expect(s().getStreaming('a2').content).toBe('from-a2');
    expect(s().isAgentStreaming('a1')).toBe(true);
    expect(s().isAgentStreaming('a2')).toBe(true);

    s().finishStreaming('a1');
    expect(s().isAgentStreaming('a1')).toBe(false);
    expect(s().isAgentStreaming('a2')).toBe(true);
    expect(s().getSession('a1').messages).toHaveLength(1);
    expect(s().getSession('a1').messages[0].content).toBe('from-a1');

    s().finishStreaming('a2');
    expect(s().isAgentStreaming('a2')).toBe(false);
    expect(s().getSession('a2').messages).toHaveLength(1);
    expect(s().getSession('a2').messages[0].content).toBe('from-a2');
  });

  it('stopStreaming of one agent does not affect another', () => {
    s().ensureSession('a1');
    s().ensureSession('a2');
    const c1 = s().startStreaming('a1');
    s().startStreaming('a2');

    s().stopStreaming('a1');
    expect(c1.signal.aborted).toBe(true);
    expect(s().isAgentStreaming('a2')).toBe(true);
  });

  it('clearSession of one agent does not affect another', () => {
    s().ensureSession('a1');
    s().ensureSession('a2');
    s().addUserMessage('a1', 'msg1');
    s().addUserMessage('a2', 'msg2');
    s().clearSession('a1');

    expect(s().sessions['a1']).toBeUndefined();
    expect(s().getSession('a2').messages).toHaveLength(1);
  });
});

// ── User messages ──

describe('addUserMessage', () => {
  it('appends user message with role, content, id, timestamp', () => {
    s().ensureSession('a1');
    s().addUserMessage('a1', 'hello');
    const msgs = s().getSession('a1').messages;
    expect(msgs).toHaveLength(1);
    expect(msgs[0].role).toBe('user');
    expect(msgs[0].content).toBe('hello');
    expect(msgs[0].id).toMatch(/^chat-/);
    expect(msgs[0].timestamp).toBeTruthy();
  });

  it('generates unique IDs', () => {
    s().ensureSession('a1');
    s().addUserMessage('a1', 'msg1');
    s().addUserMessage('a1', 'msg2');
    const [m1, m2] = s().getSession('a1').messages;
    expect(m1.id).not.toBe(m2.id);
  });

  it('works without prior ensureSession (auto-creates)', () => {
    s().addUserMessage('new-agent', 'hi');
    expect(s().getSession('new-agent').messages).toHaveLength(1);
  });
});

// ── History loading ──

describe('loadHistory', () => {
  const hist: RichMessage[] = [
    { id: 'h1', role: 'user', content: 'hello', timestamp: '2024-01-01T00:00:00Z' },
    { id: 'h2', role: 'assistant', content: 'hi', timestamp: '2024-01-01T00:00:01Z' },
  ];

  it('loads messages', () => {
    s().ensureSession('a1');
    s().loadHistory('a1', hist);
    const ses = s().getSession('a1');
    expect(ses.messages).toHaveLength(2);
    expect(ses.historyLoaded).toBe(true);
  });

  it('merges local messages when loading history before historyLoaded', () => {
    s().ensureSession('a1');
    s().addUserMessage('a1', 'existing');
    s().loadHistory('a1', hist);
    const msgs = s().getSession('a1').messages;
    expect(msgs).toHaveLength(3);
    expect(msgs[0].content).toBe('hello');
    expect(msgs[1].content).toBe('hi');
    expect(msgs[2].content).toBe('existing');
  });

  it('does not overwrite when historyLoaded is already true', () => {
    s().ensureSession('a1');
    s().loadHistory('a1', hist);
    const newer: RichMessage[] = [
      { id: 'h3', role: 'user', content: 'new', timestamp: '2024-01-01T00:00:02Z' },
    ];
    s().loadHistory('a1', newer);
    expect(s().getSession('a1').messages).toHaveLength(2);
    expect(s().getSession('a1').messages[0].content).toBe('hello');
  });

  it('loads when session has zero messages', () => {
    s().ensureSession('a1');
    s().loadHistory('a1', hist);
    expect(s().getSession('a1').messages).toHaveLength(2);
  });
});

describe('restoreFromHistory', () => {
  it('merges local-only messages with history', () => {
    s().ensureSession('a1');
    s().addUserMessage('a1', 'local-only');
    s().restoreFromHistory('a1', [
      { id: 'h1', role: 'user', content: 'restored', timestamp: '' },
    ]);

    const ses = s().getSession('a1');
    expect(ses.messages).toHaveLength(2);
    expect(ses.messages[0].content).toBe('restored');
    expect(ses.messages[1].content).toBe('local-only');
    expect(ses.historyLoaded).toBe(true);
  });

  it('does not duplicate messages already in history', () => {
    s().ensureSession('a1');
    s().restoreFromHistory('a1', [
      { id: 'h1', role: 'user', content: 'hello', timestamp: '' },
      { id: 'h2', role: 'assistant', content: 'hi', timestamp: '' },
    ]);
    s().restoreFromHistory('a1', [
      { id: 'h1', role: 'user', content: 'hello', timestamp: '' },
      { id: 'h2', role: 'assistant', content: 'hi', timestamp: '' },
    ]);

    expect(s().getSession('a1').messages).toHaveLength(2);
  });

  it('preserves streaming state when actively streaming for the same agent', () => {
    s().ensureSession('a1');
    s().startStreaming('a1');
    s().appendStreamChunk('a1', 'partial');
    s().addToolCall('a1', { name: 't', args: '', status: 'pending' });
    s().restoreFromHistory('a1', []);

    const st = s().getStreaming('a1');
    expect(st.content).toBe('partial');
    expect(st.toolCalls).toHaveLength(1);
  });

  it('clears streaming state when not actively streaming', () => {
    s().ensureSession('a1');
    s().restoreFromHistory('a1', []);

    const st = s().getStreaming('a1');
    expect(st.content).toBe('');
    expect(st.toolCalls).toHaveLength(0);
  });
});

// ── Streaming lifecycle ──

describe('streaming lifecycle', () => {
  it('startStreaming → appendStreamChunk → finishStreaming creates message', () => {
    s().ensureSession('a1');
    s().startStreaming('a1');
    expect(s().isAgentStreaming('a1')).toBe(true);

    s().appendStreamChunk('a1', 'hello ');
    s().appendStreamChunk('a1', 'world');
    expect(s().getStreaming('a1').content).toBe('hello world');

    s().finishStreaming('a1');
    expect(s().isAgentStreaming('a1')).toBe(false);
    expect(s().getStreaming('a1').content).toBe('');

    const msgs = s().getSession('a1').messages;
    expect(msgs).toHaveLength(1);
    expect(msgs[0].role).toBe('assistant');
    expect(msgs[0].content).toBe('hello world');
  });

  it('finishStreaming with no content does not create message', () => {
    s().ensureSession('a1');
    s().startStreaming('a1');
    s().finishStreaming('a1');
    expect(s().getSession('a1').messages).toHaveLength(0);
  });

  it('finishStreaming with extra merges into message', () => {
    s().ensureSession('a1');
    s().startStreaming('a1');
    s().appendStreamChunk('a1', 'callback result');
    s().finishStreaming('a1', { isCallback: true, cardId: 'card-fin' });

    const msg = s().getSession('a1').messages[0];
    expect(msg.isCallback).toBe(true);
    expect(msg.cardId).toBe('card-fin');
    expect(msg.content).toBe('callback result');
  });

  it('startStreaming returns AbortController', () => {
    s().ensureSession('a1');
    const ctrl = s().startStreaming('a1');
    expect(ctrl).toBeInstanceOf(AbortController);
    expect(s().getStreaming('a1').abortController).toBe(ctrl);
  });

  it('stopStreaming aborts controller', () => {
    s().ensureSession('a1');
    const ctrl = s().startStreaming('a1');
    s().stopStreaming('a1');
    expect(ctrl.signal.aborted).toBe(true);
  });

  it('startStreaming clears previous streaming state', () => {
    s().ensureSession('a1');
    s().startStreaming('a1');
    s().appendStreamChunk('a1', 'leftover');
    s().addToolCall('a1', { name: 'old', args: '', status: 'pending' });

    s().startStreaming('a1');
    expect(s().getStreaming('a1').content).toBe('');
    expect(s().getStreaming('a1').toolCalls).toHaveLength(0);
  });

  it('finishStreaming without startStreaming resets safely', () => {
    s().finishStreaming('a1');
    expect(s().isAgentStreaming('a1')).toBe(false);
  });
});

// ── Tool calls ──

describe('tool calls', () => {
  it('addToolCall and updateToolCallResult by name', () => {
    s().ensureSession('a1');
    s().startStreaming('a1');
    s().addToolCall('a1', { name: 'graph', args: '{"action":"update","id":"n1"}', status: 'pending' });
    expect(s().getStreaming('a1').toolCalls).toHaveLength(1);

    s().updateToolCallResult('a1', undefined, 'graph', 'ok', 'success');
    expect(s().getStreaming('a1').toolCalls[0].status).toBe('success');
    expect(s().getStreaming('a1').toolCalls[0].result).toBe('ok');
  });

  it('updateToolCallResult by id', () => {
    s().ensureSession('a1');
    s().startStreaming('a1');
    s().addToolCall('a1', { id: 'tc-1', name: 'get', args: '{}', status: 'pending' });
    s().addToolCall('a1', { id: 'tc-2', name: 'get', args: '{}', status: 'pending' });

    s().updateToolCallResult('a1', 'tc-2', 'get', 'result-2', 'success');
    expect(s().getStreaming('a1').toolCalls[0].status).toBe('pending');
    expect(s().getStreaming('a1').toolCalls[1].status).toBe('success');
    expect(s().getStreaming('a1').toolCalls[1].result).toBe('result-2');
  });

  it('updateToolCallResult only updates first pending match', () => {
    s().ensureSession('a1');
    s().startStreaming('a1');
    s().addToolCall('a1', { name: 'tool', args: '1', status: 'pending' });
    s().addToolCall('a1', { name: 'tool', args: '2', status: 'pending' });

    s().updateToolCallResult('a1', undefined, 'tool', 'done1', 'success');
    expect(s().getStreaming('a1').toolCalls[0].status).toBe('success');
    expect(s().getStreaming('a1').toolCalls[1].status).toBe('pending');
  });

  it('updateToolCallResult falls back to committed messages', () => {
    s().ensureSession('a1');
    s().startStreaming('a1');
    s().addToolCall('a1', { name: 'submit', args: '{}', status: 'pending' });
    s().commitIntermediateMessage('a1');

    expect(s().getSession('a1').messages).toHaveLength(1);
    expect(s().getSession('a1').messages[0].toolCalls![0].status).toBe('pending');

    s().updateToolCallResult('a1', undefined, 'submit', 'done', 'success');
    expect(s().getSession('a1').messages[0].toolCalls![0].status).toBe('success');
    expect(s().getSession('a1').messages[0].toolCalls![0].result).toBe('done');
  });

  it('updateToolCallResult by id falls back to committed', () => {
    s().ensureSession('a1');
    s().startStreaming('a1');
    s().addToolCall('a1', { id: 'tc-a', name: 'list', args: '{}', status: 'pending' });
    s().commitIntermediateMessage('a1');

    s().updateToolCallResult('a1', 'tc-a', 'list', 'items', 'success');
    expect(s().getSession('a1').messages[0].toolCalls![0].status).toBe('success');
  });

  it('updateToolCallResult with error status', () => {
    s().ensureSession('a1');
    s().startStreaming('a1');
    s().addToolCall('a1', { name: 'bad', args: '{}', status: 'pending' });
    s().updateToolCallResult('a1', undefined, 'bad', 'fail reason', 'error');

    expect(s().getStreaming('a1').toolCalls[0].status).toBe('error');
    expect(s().getStreaming('a1').toolCalls[0].result).toBe('fail reason');
  });

  it('finishStreaming includes tool calls in message', () => {
    s().ensureSession('a1');
    s().startStreaming('a1');
    s().addToolCall('a1', { name: 'test', args: '{}', status: 'pending' });
    s().updateToolCallResult('a1', undefined, 'test', 'res', 'success');
    s().appendStreamChunk('a1', 'analysis');
    s().finishStreaming('a1');

    const msg = s().getSession('a1').messages[0];
    expect(msg.toolCalls).toHaveLength(1);
    expect(msg.toolCalls![0].name).toBe('test');
    expect(msg.toolCalls![0].status).toBe('success');
    expect(msg.content).toBe('analysis');
  });
});

// ── commitIntermediateMessage ──

describe('commitIntermediateMessage', () => {
  it('commits streaming content and resets buffer', () => {
    s().ensureSession('a1');
    s().startStreaming('a1');
    s().appendStreamChunk('a1', 'first part');
    s().commitIntermediateMessage('a1');

    expect(s().getSession('a1').messages).toHaveLength(1);
    expect(s().getSession('a1').messages[0].content).toBe('first part');
    expect(s().getStreaming('a1').content).toBe('');
    expect(s().getStreaming('a1').toolCalls).toHaveLength(0);
  });

  it('does nothing when no content and no tool calls', () => {
    s().ensureSession('a1');
    s().startStreaming('a1');
    s().commitIntermediateMessage('a1');
    expect(s().getSession('a1').messages).toHaveLength(0);
  });

  it('commits tool calls without text content', () => {
    s().ensureSession('a1');
    s().startStreaming('a1');
    s().addToolCall('a1', { name: 't1', args: '{}', status: 'success' });
    s().commitIntermediateMessage('a1');

    const msg = s().getSession('a1').messages[0];
    expect(msg.content).toBe('');
    expect(msg.toolCalls).toHaveLength(1);
  });

  it('does nothing when agent has no streaming state', () => {
    s().commitIntermediateMessage('no-agent');
    expect(Object.keys(s().sessions)).toHaveLength(0);
  });

  it('allows multiple intermediate commits', () => {
    s().ensureSession('a1');
    s().startStreaming('a1');
    s().addToolCall('a1', { name: 'step1', args: '{}', status: 'success' });
    s().commitIntermediateMessage('a1');
    s().appendStreamChunk('a1', 'analysis');
    s().addToolCall('a1', { name: 'step2', args: '{}', status: 'pending' });
    s().commitIntermediateMessage('a1');

    expect(s().getSession('a1').messages).toHaveLength(2);
    expect(s().getSession('a1').messages[0].toolCalls![0].name).toBe('step1');
    expect(s().getSession('a1').messages[1].content).toBe('analysis');
    expect(s().getStreaming('a1').content).toBe('');
  });
});
