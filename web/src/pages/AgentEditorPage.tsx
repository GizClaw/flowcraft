import { useEffect, useRef, useState } from 'react';
import { useOutletContext } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import WorkflowEditor from '../components/WorkflowEditor';
import Sidebar from '../components/Sidebar';
import NodeConfigPanel from '../components/NodeConfigPanel';
import EdgeConfigPanel from '../components/EdgeConfigPanel';
import ImportExportDialog from '../components/workflow/ImportExportDialog';
import { useWorkflowStore } from '../store/workflowStore';
import { useNodeTypeStore } from '../store/nodeTypeStore';
import { useCoPilotStore } from '../store/copilotStore';
import { compileApi, agentApi } from '../utils/api';
import { graphDefToReactFlow, toGraphDefinition, applyDagreLayout } from '../utils/nodeHelpers';
import { parseCompileResult, parseDryRunResult, countDryRunIssues } from '../utils/compileHelpers';
import { useToastStore } from '../store/toastStore';
import { Save, AlertTriangle } from 'lucide-react';
import type { Agent } from '../types/app';

export default function AgentEditorPage() {
  const { t } = useTranslation();
  const { agent, setAgent } = useOutletContext<{ agent: Agent; setAgent: (a: Agent) => void }>();
  const nodes = useWorkflowStore((s) => s.nodes);
  const edges = useWorkflowStore((s) => s.edges);
  const isDirty = useWorkflowStore((s) => s.isDirty);
  const setDirty = useWorkflowStore((s) => s.setDirty);
  const selectedNodeId = useWorkflowStore((s) => s.selectedNodeId);
  const selectedEdgeId = useWorkflowStore((s) => s.selectedEdgeId);
  const setNodeWarnings = useWorkflowStore((s) => s.setNodeWarnings);
  const setNodeErrors = useWorkflowStore((s) => s.setNodeErrors);
  const fetchNodeTypes = useNodeTypeStore((s) => s.fetchNodeTypes);
  const fetched = useNodeTypeStore((s) => s.fetched);
  const setCurrentAgentId = useCoPilotStore((s) => s.setCurrentAgentId);
  const addToast = useToastStore((s) => s.addToast);
  const [autoLayoutOnSave, setAutoLayoutOnSave] = useState(false);
  const skipNextGraphLoad = useRef(false);

  useEffect(() => {
    if (!fetched) fetchNodeTypes();
  }, [fetched, fetchNodeTypes]);

  useEffect(() => {
    setCurrentAgentId(agent.id);
    return () => setCurrentAgentId(null);
  }, [agent.id, setCurrentAgentId]);

  useEffect(() => {
    if (skipNextGraphLoad.current) {
      skipNextGraphLoad.current = false;
      return;
    }
    if (agent.graph_definition) {
      const { nodes: n, edges: e } = graphDefToReactFlow(agent.graph_definition);
      useWorkflowStore.getState().loadGraph(n, e);
    } else {
      useWorkflowStore.getState().reset();
    }
  }, [agent]);

  const handleSave = async () => {
    let saveNodes = nodes;
    if (autoLayoutOnSave) {
      saveNodes = applyDagreLayout(nodes, edges);
      useWorkflowStore.getState().loadGraph(saveNodes, edges);
    }
    const graphDef = toGraphDefinition(saveNodes, edges, agent.name);
    try {
      skipNextGraphLoad.current = true;
      const updated = await agentApi.update(agent.id, { graph_definition: graphDef });
      setAgent(updated);
      setDirty(false);
      addToast('success', t('editor.saved'));

      const result = await compileApi.compile(agent.id);
      const { errors: errMap, warnings: warnMap } = parseCompileResult(result);
      setNodeWarnings(warnMap);
      setNodeErrors(errMap);
      if (result.errors && result.errors.length > 0) {
        addToast('warning', t('editor.compileErrors', { count: result.errors.length, message: result.errors[0].message }));
      }
    } catch (err) {
      skipNextGraphLoad.current = false;
      addToast('error', err instanceof Error ? err.message : t('editor.saveFailed'));
    }
  };

  const handleDryRun = async () => {
    try {
      const result = await compileApi.dryrun(agent.id);
      const { errors: errMap, warnings: warnMap } = parseDryRunResult(result, t('editor.nodeInvalid'));
      setNodeErrors(errMap);
      setNodeWarnings(warnMap);
      const issueCount = countDryRunIssues(result);
      if (issueCount > 0) {
        addToast('warning', t('editor.dryRunIssues', { count: issueCount }));
      } else {
        addToast('success', t('editor.dryRunPassed'));
      }
    } catch (err) {
      addToast('error', err instanceof Error ? err.message : t('editor.dryRunFailed'));
    }
  };

  const handleRefresh = async () => {
    const updated = await agentApi.get(agent.id);
    setAgent(updated);
  };

  return (
    <div className="flex h-full">
      <Sidebar />
      <div className="flex-1 flex flex-col min-w-0">
        <div className="flex items-center gap-2 px-4 py-2 border-b border-gray-200 dark:border-gray-800 bg-white dark:bg-gray-900 shrink-0">
          <button onClick={handleSave} disabled={!isDirty} className="flex items-center gap-1 px-3 py-1.5 text-sm bg-indigo-600 text-white rounded-lg hover:bg-indigo-700 disabled:opacity-50">
            <Save size={14} /> {t('editor.save')}
          </button>
          <button onClick={handleDryRun} className="flex items-center gap-1 px-3 py-1.5 text-sm border border-gray-300 dark:border-gray-600 rounded-lg hover:bg-gray-50 dark:hover:bg-gray-800">
            <AlertTriangle size={14} /> {t('editor.dryRun')}
          </button>
          <ImportExportDialog agentId={agent.id} onImportSuccess={handleRefresh} />
          <label className="flex items-center gap-1.5 ml-2 text-xs text-gray-500 dark:text-gray-400 select-none cursor-pointer">
            <input type="checkbox" checked={autoLayoutOnSave} onChange={(e) => setAutoLayoutOnSave(e.target.checked)} className="rounded border-gray-300 dark:border-gray-600 text-indigo-600 focus:ring-indigo-500" />
            {t('editor.autoLayout')}
          </label>
          <div className="flex-1" />
          {isDirty && <span className="text-xs text-amber-500">{t('editor.unsaved')}</span>}
        </div>
        <div className="flex-1 flex">
          <div className="flex-1">
            <WorkflowEditor />
          </div>
          {selectedNodeId && <NodeConfigPanel />}
          {selectedEdgeId && !selectedNodeId && <EdgeConfigPanel />}
        </div>
      </div>
    </div>
  );
}
