import { useCallback } from 'react';
import { useCoPilotStore, COPILOT_AGENT_ID } from '../store/copilotStore';
import { useChatStore } from '../store/chatStore';
import { useWorkflowStore } from '../store/workflowStore';
import { graphDefToReactFlow } from '../utils/nodeHelpers';
import { useChat } from './useChat';
import { agentApi } from '../utils/api';
import type { CoPilotContextInput, CoPilotRef, AgentToolResultEvent, DispatchedTask } from '../types/chat';

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

  const tryTrackDispatchedTask = useCallback((event: AgentToolResultEvent) => {
    const resultStr = event.tool_result;
    if (!resultStr) return;
    try {
      const parsed = JSON.parse(resultStr);
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

  const onToolResult = useCallback((event: AgentToolResultEvent) => {
    if (event.tool_name && GRAPH_MUTATING_TOOLS.includes(event.tool_name)) {
      refreshGraph();
    }
    if (event.tool_name === 'kanban_submit') {
      tryTrackDispatchedTask(event);
    }
  }, [refreshGraph, tryTrackDispatchedTask]);

  const onDone = useCallback(() => {
    setBackgroundRunning(false);
  }, [setBackgroundRunning]);

  const { sendMessage, stopStreaming, approval, submitApproval } = useChat({
    agentId: COPILOT_AGENT_ID,
    buildRequest,
    onToolResult,
    onDone,
  });

  return { sendMessage, stopStreaming, approval, submitApproval };
}
