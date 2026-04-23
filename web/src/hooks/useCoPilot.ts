import { useCallback, useEffect } from 'react';
import { useCoPilotStore, COPILOT_AGENT_ID } from '../store/copilotStore';
import { useChatStore } from '../store/chatStore';
import { useWorkflowStore } from '../store/workflowStore';
import { graphDefToReactFlow } from '../utils/nodeHelpers';
import { useChat } from './useChat';
import { agentApi } from '../utils/api';
import type { CoPilotContextInput, CoPilotRef, DispatchedTask } from '../types/chat';
import { envelopeRouter } from '../eventlog/router';
import { useEventStore } from '../store/eventStore';
import type { Envelope } from '../eventlog/types';
import { getRuntimeConversationId } from '../utils/runtime';

const GRAPH_MUTATING_TOOLS = ['graph'];

function extractRefs(message: string): CoPilotRef[] {
  const refs: CoPilotRef[] = [];
  const regex = /\[ref:node:([^\]]+)\]/g;
  let match;
  const seen = new Set<string>();
  while ((match = regex.exec(message)) !== null) {
    const nodeId = match[1];
    if (!seen.has(nodeId)) {
      seen.add(nodeId);
      refs.push({ type: 'node', id: nodeId });
    }
  }
  return refs;
}

export function useCoPilot() {
  const currentAgentId = useCoPilotStore((s) => s.currentAgentId);
  const graphContext = useCoPilotStore((s) => s.graphContext);
  const setBackgroundRunning = useCoPilotStore((s) => s.setBackgroundRunning);
  const trackDispatchedTask = useCoPilotStore((s) => s.trackDispatchedTask);
  const commitIntermediateMessage = useChatStore((s) => s.commitIntermediateMessage);
  const loadGraph = useWorkflowStore((s) => s.loadGraph);

  const buildRequest = useCallback((content: string) => {
    const refs = extractRefs(content);
    const copilotContext: CoPilotContextInput = {
      ...(currentAgentId ? { current_agent_id: currentAgentId } : {}),
      ...(refs.length > 0 ? { refs } : {}),
      ...(graphContext.nodeCount > 0 ? {
        graph_context: {
          node_count: graphContext.nodeCount,
          edge_count: graphContext.edgeCount,
          node_types: graphContext.nodeTypes,
          summary: graphContext.summary,
        },
      } : {}),
    };
    return {
      inputs: {
        copilot_context: copilotContext,
      },
    };
  }, [currentAgentId, graphContext]);

  const refreshGraph = useCallback(async () => {
    if (!currentAgentId) return;
    try {
      const agent = await agentApi.get(currentAgentId);
      if (agent.graph_definition) {
        const { nodes, edges } = graphDefToReactFlow(agent.graph_definition);
        loadGraph(nodes, edges);
      }
    } catch { /* best effort */ }
  }, [currentAgentId, loadGraph]);

  const tryTrackDispatchedTask = useCallback((toolOutput: string) => {
    if (!toolOutput) return;
    try {
      const parsed = JSON.parse(toolOutput);
      if (parsed.card_id) {
        const task: DispatchedTask = {
          cardId: parsed.card_id,
          template: parsed.target_agent_id || parsed.template || 'unknown',
          status: 'submitted',
        };
        commitIntermediateMessage(COPILOT_AGENT_ID);
        trackDispatchedTask(task);
      }
    } catch { /* not async dispatch result */ }
  }, [commitIntermediateMessage, trackDispatchedTask]);

  // The legacy useChat onToolResult/onDone callbacks are gone — every
  // observable agent event now flows through the global EnvelopeClient.
  // Subscribe directly to the same router for the side-effects CoPilot
  // needs (refresh graph after a graph-mutating tool, track dispatched
  // tasks from kanban_submit, clear background-running indicator).
  // §13 / Track-A: subscribe to the CoPilot card partition so the
  // EnvelopeClient actually pulls agent.tool.* / agent.stream.delta /
  // agent.run.* envelopes for the CoPilot conversation. Without this the
  // router subscriptions below would be wired to a silent stream.
  useEffect(() => {
    const conversationId = getRuntimeConversationId(COPILOT_AGENT_ID);
    return useEventStore.getState().trackSubscribe(`card:${conversationId}`);
  }, []);

  useEffect(() => {
    const conversationId = getRuntimeConversationId(COPILOT_AGENT_ID);

    const onToolReturned = (env: Envelope) => {
      const p = env.payload as { tool_name?: string; output?: string; conversation_id?: string };
      if (p.conversation_id && p.conversation_id !== conversationId) return;
      if (!p.tool_name) return;
      if (GRAPH_MUTATING_TOOLS.includes(p.tool_name)) refreshGraph();
      if (p.tool_name === 'kanban_submit' && p.output) tryTrackDispatchedTask(p.output);
    };

    const onRunSettled = (env: Envelope) => {
      const p = env.payload as { conversation_id?: string };
      if (p.conversation_id && p.conversation_id !== conversationId) return;
      setBackgroundRunning(false);
    };

    const off1 = envelopeRouter.on('agent.tool.returned', onToolReturned);
    const off2 = envelopeRouter.on('agent.run.completed', onRunSettled);
    const off3 = envelopeRouter.on('agent.run.failed', onRunSettled);
    return () => { off1(); off2(); off3(); };
  }, [refreshGraph, tryTrackDispatchedTask, setBackgroundRunning]);

  const { sendMessage, stopStreaming, approval, submitApproval } = useChat({
    agentId: COPILOT_AGENT_ID,
    buildRequest,
  });

  return { sendMessage, stopStreaming, approval, submitApproval };
}
