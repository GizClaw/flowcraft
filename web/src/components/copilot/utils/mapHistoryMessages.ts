import type { RichMessage, DispatchedTask } from '../../../types/chat';
import type { Message } from '../../../types/chat';

interface ToolCallMeta { id: string; name: string; arguments: string }
interface ToolResultMeta { tool_call_id: string; content: string }

export function mapHistoryMessages(raw: Message[]): RichMessage[] {
  const toolResultMap = new Map<string, string>();
  for (const m of raw) {
    if (m.role !== 'tool') continue;
    const results = (m.metadata?.tool_result ?? []) as ToolResultMeta[];
    for (const tr of results) {
      toolResultMap.set(tr.tool_call_id, tr.content);
    }
  }

  const result: RichMessage[] = [];
  for (const m of raw) {
    if (m.role === 'system' || m.role === 'tool') continue;

    const msg: RichMessage = {
      id: m.id || `hist-${result.length}`,
      role: m.role as 'user' | 'assistant',
      content: m.content,
      timestamp: m.created_at,
    };

    if (m.role === 'assistant' && m.metadata?.tool_calls) {
      const calls = m.metadata.tool_calls as ToolCallMeta[];
      if (calls.length > 0) {
        msg.toolCalls = calls.map((c) => {
          const res = toolResultMap.get(c.id);
          return {
            id: c.id,
            name: c.name,
            args: c.arguments || '',
            result: res,
            status: res !== undefined ? 'success' as const : 'pending' as const,
          };
        });
      }
    }

    if (m.metadata?.callback) {
      msg.isCallback = true;
      if (m.metadata.card_id) {
        msg.cardId = m.metadata.card_id as string;
      }
    }

    if (m.metadata?.dispatched_task) {
      msg.dispatchedTask = m.metadata.dispatched_task as DispatchedTask;
    }

    result.push(msg);
  }

  return result;
}
