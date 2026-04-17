import { describe, it, expect } from 'vitest';
import { mapHistoryMessages } from './mapHistoryMessages';
import type { Message } from '../../../types/chat';

function msg(overrides: Partial<Message>): Message {
  return {
    id: 'msg-1',
    conversation_id: 'conv-1',
    role: 'user',
    content: 'hello',
    created_at: '2024-01-01T00:00:00Z',
    ...overrides,
  };
}

describe('mapHistoryMessages', () => {
  it('converts user and assistant messages', () => {
    const raw: Message[] = [
      msg({ id: 'm1', role: 'user', content: 'hi' }),
      msg({ id: 'm2', role: 'assistant', content: 'hello' }),
    ];
    const result = mapHistoryMessages(raw);
    expect(result).toHaveLength(2);
    expect(result[0].role).toBe('user');
    expect(result[1].role).toBe('assistant');
  });

  it('filters out system and tool messages', () => {
    const raw: Message[] = [
      msg({ id: 'm1', role: 'system', content: 'sys' }),
      msg({ id: 'm2', role: 'user', content: 'hi' }),
      msg({ id: 'm3', role: 'tool', content: '', metadata: { tool_result: [] } }),
    ];
    const result = mapHistoryMessages(raw);
    expect(result).toHaveLength(1);
    expect(result[0].role).toBe('user');
  });

  it('maps tool calls with results as success', () => {
    const raw: Message[] = [
      msg({
        id: 'm1', role: 'assistant', content: 'let me check',
        metadata: {
          tool_calls: [{ id: 'tc-1', name: 'graph', arguments: '{"action":"get","agent_id":"g1"}' }],
        },
      }),
      msg({
        id: 'm2', role: 'tool', content: '',
        metadata: {
          tool_result: [{ tool_call_id: 'tc-1', content: '{"nodes":[]}' }],
        },
      }),
    ];
    const result = mapHistoryMessages(raw);
    expect(result).toHaveLength(1);
    expect(result[0].toolCalls).toHaveLength(1);
    expect(result[0].toolCalls![0].id).toBe('tc-1');
    expect(result[0].toolCalls![0].name).toBe('graph');
    expect(result[0].toolCalls![0].status).toBe('success');
    expect(result[0].toolCalls![0].result).toBe('{"nodes":[]}');
  });

  it('maps tool calls without results as pending', () => {
    const raw: Message[] = [
      msg({
        id: 'm1', role: 'assistant', content: '',
        metadata: {
          tool_calls: [{ id: 'tc-missing', name: 'slow_tool', arguments: '{}' }],
        },
      }),
    ];
    const result = mapHistoryMessages(raw);
    expect(result[0].toolCalls![0].status).toBe('pending');
    expect(result[0].toolCalls![0].result).toBeUndefined();
  });

  it('sets isCallback and cardId when metadata.callback is truthy', () => {
    const raw: Message[] = [
      msg({ id: 'm1', role: 'assistant', content: 'callback result', metadata: { callback: true, card_id: 'card-xyz' } }),
    ];
    const result = mapHistoryMessages(raw);
    expect(result[0].isCallback).toBe(true);
    expect(result[0].cardId).toBe('card-xyz');
  });

  it('generates fallback IDs when id is missing', () => {
    const raw: Message[] = [
      msg({ id: '', role: 'user', content: 'no id' }),
    ];
    const result = mapHistoryMessages(raw);
    expect(result[0].id).toBe('hist-0');
  });

  it('handles multiple tool calls with mixed results', () => {
    const raw: Message[] = [
      msg({
        id: 'm1', role: 'assistant', content: '',
        metadata: {
          tool_calls: [
            { id: 'tc-1', name: 'graph', arguments: '{"action":"get"}' },
            { id: 'tc-2', name: 'agent', arguments: '{}' },
          ],
        },
      }),
      msg({
        id: 'm2', role: 'tool', content: '',
        metadata: {
          tool_result: [{ tool_call_id: 'tc-1', content: 'graph data' }],
        },
      }),
    ];
    const result = mapHistoryMessages(raw);
    expect(result[0].toolCalls).toHaveLength(2);
    expect(result[0].toolCalls![0].status).toBe('success');
    expect(result[0].toolCalls![1].status).toBe('pending');
  });

  it('maps dispatched_task from metadata', () => {
    const raw: Message[] = [
      msg({
        id: 'm1', role: 'assistant', content: '',
        metadata: {
          dispatched_task: { cardId: 'card-1', template: 'copilot_builder', status: 'submitted' },
        },
      }),
    ];
    const result = mapHistoryMessages(raw);
    expect(result[0].dispatchedTask).toBeDefined();
    expect(result[0].dispatchedTask!.cardId).toBe('card-1');
    expect(result[0].dispatchedTask!.template).toBe('copilot_builder');
    expect(result[0].dispatchedTask!.status).toBe('submitted');
  });

  it('handles message with both callback and tool_calls', () => {
    const raw: Message[] = [
      msg({
        id: 'm1', role: 'assistant', content: 'callback with tools',
        metadata: {
          callback: true,
          tool_calls: [{ id: 'tc-1', name: 'tool_a', arguments: '{}' }],
        },
      }),
      msg({
        id: 'm2', role: 'tool', content: '',
        metadata: { tool_result: [{ tool_call_id: 'tc-1', content: 'done' }] },
      }),
    ];
    const result = mapHistoryMessages(raw);
    expect(result[0].isCallback).toBe(true);
    expect(result[0].toolCalls).toHaveLength(1);
    expect(result[0].toolCalls![0].status).toBe('success');
  });

  it('handles empty tool_calls array', () => {
    const raw: Message[] = [
      msg({ id: 'm1', role: 'assistant', content: 'no tools', metadata: { tool_calls: [] } }),
    ];
    const result = mapHistoryMessages(raw);
    expect(result[0].toolCalls).toBeUndefined();
  });

  it('preserves tool_call arguments', () => {
    const raw: Message[] = [
      msg({
        id: 'm1', role: 'assistant', content: '',
        metadata: {
          tool_calls: [{ id: 'tc-1', name: 'search', arguments: '{"query":"test","limit":10}' }],
        },
      }),
    ];
    const result = mapHistoryMessages(raw);
    expect(result[0].toolCalls![0].args).toBe('{"query":"test","limit":10}');
  });

  it('handles tool_calls with empty arguments', () => {
    const raw: Message[] = [
      msg({
        id: 'm1', role: 'assistant', content: '',
        metadata: { tool_calls: [{ id: 'tc-1', name: 'no_args', arguments: '' }] },
      }),
    ];
    const result = mapHistoryMessages(raw);
    expect(result[0].toolCalls![0].args).toBe('');
  });

  it('handles multiple tool results in single tool message', () => {
    const raw: Message[] = [
      msg({
        id: 'm1', role: 'assistant', content: '',
        metadata: {
          tool_calls: [
            { id: 'tc-1', name: 'tool_a', arguments: '{}' },
            { id: 'tc-2', name: 'tool_b', arguments: '{}' },
          ],
        },
      }),
      msg({
        id: 'm2', role: 'tool', content: '',
        metadata: {
          tool_result: [
            { tool_call_id: 'tc-1', content: 'result-a' },
            { tool_call_id: 'tc-2', content: 'result-b' },
          ],
        },
      }),
    ];
    const result = mapHistoryMessages(raw);
    expect(result[0].toolCalls![0].result).toBe('result-a');
    expect(result[0].toolCalls![0].status).toBe('success');
    expect(result[0].toolCalls![1].result).toBe('result-b');
    expect(result[0].toolCalls![1].status).toBe('success');
  });

  it('returns empty array for empty input', () => {
    expect(mapHistoryMessages([])).toEqual([]);
  });

  it('returns empty for all-system messages', () => {
    const raw: Message[] = [
      msg({ role: 'system', content: 'sys prompt' }),
    ];
    expect(mapHistoryMessages(raw)).toEqual([]);
  });
});
