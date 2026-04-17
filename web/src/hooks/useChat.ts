import { useCallback, useMemo, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { useChatStore } from '../store/chatStore';
import { chatApi } from '../utils/api';
import type { WorkflowStreamEvent, AgentToolResultEvent, StreamDoneEvent, ChatRequest, ApprovalRequiredEvent } from '../types/chat';
import { useNotification } from './useNotification';
import { getRuntimeConversationId } from '../utils/runtime';

export interface ApprovalState {
  pending: boolean;
  runId: string;
  conversationId: string;
  state: Record<string, unknown> | null;
  prompt?: string;
}

interface UseChatOptions {
  agentId: string;
  streamApi?: (request: ChatRequest, signal?: AbortSignal) => AsyncIterable<WorkflowStreamEvent>;
  onToolResult?: (event: AgentToolResultEvent) => void;
  onDone?: (event: StreamDoneEvent) => void;
  buildRequest?: (content: string) => Partial<ChatRequest>;
}

export function useChat(options: UseChatOptions) {
  const { t } = useTranslation();
  const { agentId, onToolResult, onDone, buildRequest } = options;
  const streamFn = useMemo(
    () => options.streamApi || ((req: ChatRequest) => chatApi.stream(req)),
    [options.streamApi],
  );

  const addUserMessage = useChatStore((s) => s.addUserMessage);
  const startStreaming = useChatStore((s) => s.startStreaming);
  const appendStreamChunk = useChatStore((s) => s.appendStreamChunk);
  const finishStreaming = useChatStore((s) => s.finishStreaming);
  const stopStreaming = useChatStore((s) => s.stopStreaming);
  const addToolCall = useChatStore((s) => s.addToolCall);
  const updateToolCallResult = useChatStore((s) => s.updateToolCallResult);
  const commitIntermediateMessage = useChatStore((s) => s.commitIntermediateMessage);
  const { handleStreamEvent } = useNotification();

  const [approval, setApproval] = useState<ApprovalState>({
    pending: false, runId: '', conversationId: '', state: null,
  });

  const sendMessage = useCallback(async (content: string) => {
    addUserMessage(agentId, content);
    const controller = startStreaming(agentId);

    const extra = buildRequest?.(content) || {};

    let isCallback = false;
    let callbackCardId = '';
    let tokenBuffer = '';
    let tokenTimer: ReturnType<typeof setTimeout> | null = null;

    function flushTokens() {
      if (tokenBuffer) {
        appendStreamChunk(agentId, tokenBuffer);
        tokenBuffer = '';
      }
      if (tokenTimer) { clearTimeout(tokenTimer); tokenTimer = null; }
    }

    try {
      const extraInputs = (extra.inputs && typeof extra.inputs === 'object')
        ? extra.inputs
        : undefined;
      const request: ChatRequest = {
        ...extra,
        agent_id: agentId,
        query: content,
        conversation_id: getRuntimeConversationId(agentId),
        ...(extraInputs ? { inputs: extraInputs } : {}),
      };

      const stream = streamFn(request, controller.signal);

      for await (const event of stream) {
        if (controller.signal.aborted) break;
        handleStreamEvent(event);
        handleEvent(event);
      }
    } catch (err) {
      flushTokens();
      if (err instanceof DOMException && err.name === 'AbortError') {
        appendStreamChunk(agentId, t('chat.aborted'));
      } else {
        appendStreamChunk(agentId, `Error: ${err instanceof Error ? err.message : 'Unknown error'}`);
      }
    } finally {
      flushTokens();
      finishStreaming(agentId, isCallback ? { isCallback: true, cardId: callbackCardId || undefined } : undefined);
    }

    function handleEvent(event: WorkflowStreamEvent) {
      const st = useChatStore.getState().getStreaming(agentId);

      switch (event.type) {
        case 'agent_token':
          if (event.chunk) {
            if (st.toolCalls.length > 0 && st.toolCalls.every((tc) => tc.status !== 'pending')) {
              flushTokens();
              commitIntermediateMessage(agentId);
            }
            tokenBuffer += event.chunk;
            if (!tokenTimer) {
              tokenTimer = setTimeout(flushTokens, 16);
            }
          }
          break;
        case 'agent_tool_call':
          flushTokens();
          if (event.tool_name) {
            if (useChatStore.getState().getStreaming(agentId).content) {
              commitIntermediateMessage(agentId);
            }
            addToolCall(agentId, { id: event.tool_call_id, name: event.tool_name, args: event.tool_args || '', status: 'pending' });
          }
          break;
        case 'agent_tool_result':
          if (event.tool_name) {
            updateToolCallResult(agentId, event.tool_call_id, event.tool_name, event.tool_result || '', event.is_error ? 'error' : 'success');
            onToolResult?.(event as AgentToolResultEvent);
          }
          break;
        case 'approval_required': {
          flushTokens();
          const approvalEvent = event as ApprovalRequiredEvent;
          setApproval({
            pending: true,
            runId: approvalEvent.run_id || '',
            conversationId: approvalEvent.conversation_id || getRuntimeConversationId(agentId),
            state: (approvalEvent.data as Record<string, unknown>) || null,
            prompt: approvalEvent.prompt,
          });
          break;
        }
        case 'done': {
          flushTokens();
          const done = event as StreamDoneEvent;
          const doneAny = event as unknown as Record<string, unknown>;
          const metadata = doneAny.metadata as Record<string, unknown> | undefined;
          if (metadata?.callback) {
            isCallback = true;
            callbackCardId = (metadata.card_id as string) || '';
          }
          if (done.status === 'interrupted' && done.output?.state) {
            setApproval({
              pending: true,
              runId: done.output.run_id || done.run_id || '',
              conversationId: done.output.conversation_id || done.conversation_id || getRuntimeConversationId(agentId),
              state: done.output.state as Record<string, unknown>,
            });
          }
          onDone?.(done);
          break;
        }
        case 'error':
          flushTokens();
          appendStreamChunk(agentId, `Error: ${event.error || event.message || 'Unknown error'}`);
          break;
      }
    }
  }, [agentId, streamFn, addUserMessage, startStreaming, appendStreamChunk, finishStreaming, addToolCall, updateToolCallResult, commitIntermediateMessage, buildRequest, onToolResult, onDone, handleStreamEvent, t]);

  const stop = useCallback(() => stopStreaming(agentId), [agentId, stopStreaming]);

  const submitApproval = useCallback(async (decision: 'approved' | 'rejected', comment?: string) => {
    if (!approval.pending || !approval.state) return;
    setApproval((prev) => ({ ...prev, pending: false }));

    const controller = startStreaming(agentId);
    try {
      const stream = chatApi.resumeStream(agentId, {
        conversation_id: approval.conversationId,
        run_id: approval.runId,
        state: approval.state,
        decision: {
          approval_decision: decision,
          ...(comment ? { approval_comment: comment } : {}),
        },
      });
      for await (const event of stream) {
        if (controller.signal.aborted) break;
        handleStreamEvent(event);
        // Re-use the same event dispatch — approval_required can recur
      }
    } catch (err) {
      appendStreamChunk(agentId, `Error: ${err instanceof Error ? err.message : 'Unknown error'}`);
    } finally {
      finishStreaming(agentId);
    }
  }, [agentId, approval, startStreaming, finishStreaming, appendStreamChunk, handleStreamEvent]);

  return { sendMessage, stopStreaming: stop, approval, submitApproval };
}
