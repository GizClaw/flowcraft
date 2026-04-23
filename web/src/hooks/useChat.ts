import { useCallback, useEffect, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { useChatStore } from '../store/chatStore';
import { chatApi } from '../utils/api';
import type { ApprovalRequiredEvent } from '../types/chat';
import { useNotification } from './useNotification';
import { getRuntimeConversationId } from '../utils/runtime';
import { envelopeRouter } from '../eventlog/router';
import type { Envelope } from '../eventlog/types';

const EVT_APPROVAL_REQUIRED = 'approval.required';
const EVT_RUN_COMPLETED = 'agent.run.completed';
const EVT_RUN_FAILED = 'agent.run.failed';

export interface ApprovalState {
  pending: boolean;
  runId: string;
  conversationId: string;
  state: Record<string, unknown> | null;
  prompt?: string;
}

interface UseChatOptions {
  agentId: string;
  buildRequest?: (content: string) => { inputs?: Record<string, unknown>; async?: boolean };
}

// useChat is now a thin command-side hook:
//   * sendMessage    → POST /api/conversations/{id}/runs (no streaming)
//   * submitApproval → POST /api/conversations/{id}/approval
// All progress (agent.stream.delta, kanban.card.*, agent.run.completed,
// approval.required) flows through the global EnvelopeClient and is
// projected into chatStore by chatReducers. The hook only watches the same
// envelope stream to surface HITL approval prompts as React state for the
// approval modal.
export function useChat(options: UseChatOptions) {
  const { t } = useTranslation();
  const { agentId, buildRequest } = options;

  const startStreaming = useChatStore((s) => s.startStreaming);
  const appendStreamChunk = useChatStore((s) => s.appendStreamChunk);
  const finishStreaming = useChatStore((s) => s.finishStreaming);
  const stopStreaming = useChatStore((s) => s.stopStreaming);
  const { handleStreamEvent } = useNotification();

  const [approval, setApproval] = useState<ApprovalState>({
    pending: false, runId: '', conversationId: '', state: null,
  });

  const conversationId = getRuntimeConversationId(agentId);

  // Surface approval-required envelopes for this conversation as React state.
  useEffect(() => {
    const handler = (env: Envelope) => {
      const payload = env.payload as { run_id?: string; conversation_id?: string; conversationID?: string; state?: Record<string, unknown>; prompt?: string };
      const convID = payload.conversationID ?? payload.conversation_id;
      if (convID && convID !== conversationId) return;
      setApproval({
        pending: true,
        runId: payload.run_id || '',
        conversationId: convID || conversationId,
        state: payload.state || null,
        prompt: payload.prompt,
      });
      handleStreamEvent({ type: 'approval_required', ...payload } as ApprovalRequiredEvent);
    };
    const dispose = envelopeRouter.on(EVT_APPROVAL_REQUIRED, handler);
    return () => { dispose(); };
  }, [conversationId, handleStreamEvent]);

  // Close the streaming indicator once the run terminates.
  useEffect(() => {
    const handler = (env: Envelope) => {
      const payload = env.payload as { conversation_id?: string; conversationID?: string };
      const convID = payload.conversationID ?? payload.conversation_id;
      if (convID && convID !== conversationId) return;
      finishStreaming(conversationId);
    };
    const dispose1 = envelopeRouter.on(EVT_RUN_COMPLETED, handler);
    const dispose2 = envelopeRouter.on(EVT_RUN_FAILED, handler);
    return () => { dispose1(); dispose2(); };
  }, [conversationId, finishStreaming]);

  const sendMessage = useCallback(async (content: string) => {
    startStreaming(conversationId);
    const extra = buildRequest?.(content) || {};
    try {
      await chatApi.startRun(conversationId, {
        agent_id: agentId,
        query: content,
        ...(extra.inputs ? { inputs: extra.inputs } : {}),
        ...(extra.async !== undefined ? { async: extra.async } : {}),
      });
      // chat.message.sent for the user prompt is published by the backend
      // ChatSendCommand and projected into the store by chatReducers; the
      // assistant reply streams in via agent.stream.delta envelopes.
    } catch (err) {
      appendStreamChunk(conversationId, `${t('chat.error')}: ${err instanceof Error ? err.message : 'Unknown error'}`);
      finishStreaming(conversationId);
    }
  }, [agentId, conversationId, buildRequest, startStreaming, appendStreamChunk, finishStreaming, t]);

  const stop = useCallback(() => stopStreaming(conversationId), [conversationId, stopStreaming]);

  // submitApproval POSTs the decision; the resumed agent's tokens and tool
  // events arrive via the global EnvelopeClient subscription. We start a
  // streaming session so the UI shows "agent typing" until the first
  // envelope closes it.
  const submitApproval = useCallback(async (decision: 'approved' | 'rejected', comment?: string) => {
    if (!approval.pending || !approval.runId) return;
    setApproval((prev) => ({ ...prev, pending: false }));
    startStreaming(conversationId);
    try {
      await chatApi.submitApproval(approval.conversationId, {
        agent_id: agentId,
        run_id: approval.runId,
        decision,
        ...(comment ? { comment } : {}),
        ...(approval.state ? { state: approval.state } : {}),
      });
    } catch (err) {
      appendStreamChunk(conversationId, `Error: ${err instanceof Error ? err.message : 'Unknown error'}`);
      finishStreaming(conversationId);
    }
  }, [agentId, conversationId, approval, startStreaming, finishStreaming, appendStreamChunk]);

  return { sendMessage, stopStreaming: stop, approval, submitApproval };
}
