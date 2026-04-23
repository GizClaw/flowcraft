import { useOutletContext } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import type { Agent } from '../types/app';

function ApiBlock({ method, path, body }: { method: string; path: string; body?: string }) {
  const color = method === 'POST' ? 'text-amber-600 dark:text-amber-400' : 'text-green-600 dark:text-green-400';
  return (
    <code className="block text-xs bg-gray-50 dark:bg-gray-800 p-3 rounded-lg font-mono leading-relaxed">
      <span className={color}>{method}</span>{' '}
      <span className="text-indigo-600 dark:text-indigo-400">{path}</span>
      {body && <span className="text-gray-500 dark:text-gray-400">{'\n'}{body}</span>}
    </code>
  );
}

function Section({ title, children }: { title: string; children: React.ReactNode }) {
  return (
    <div className="bg-white dark:bg-gray-900 rounded-xl border border-gray-200 dark:border-gray-800 p-4 space-y-3">
      <h3 className="text-sm font-semibold text-gray-700 dark:text-gray-300">{title}</h3>
      {children}
    </div>
  );
}

export default function AgentApiPage() {
  const { t } = useTranslation();
  const { agent } = useOutletContext<{ agent: Agent }>();
  const id = agent.id;

  return (
    <div className="p-6 max-w-3xl mx-auto space-y-6">
      <h2 className="text-lg font-semibold text-gray-900 dark:text-gray-100">{t('apiRef.title')}</h2>

      <Section title={t('apiRef.chatStream')}>
        <ApiBlock
          method="POST" path="/api/chat/stream"
          body={`{ "agent_id": "${id}", "query": "...", "inputs": {} }`}
        />
        <p className="text-xs text-gray-500">{t('apiRef.chatStreamDesc')}</p>
      </Section>

      <Section title={t('apiRef.resume')}>
        <ApiBlock
          method="POST" path="/api/conversations/{id}/approval"
          body={`{ "agent_id": "${id}", "run_id": "...", "decision": "approved" }`}
        />
        <p className="text-xs text-gray-500">{t('apiRef.resumeDesc')}</p>
      </Section>

      <Section title={t('apiRef.workflowRuns')}>
        <div className="space-y-2">
          <ApiBlock method="GET" path={`/api/workflows/runs?agent_id=${id}`} />
          <ApiBlock method="GET" path="/api/workflows/runs/{run_id}" />
          <ApiBlock method="GET" path="/api/workflows/runs/{run_id}/status" />
        </div>
      </Section>

      <Section title={t('apiRef.conversations')}>
        <div className="space-y-2">
          <ApiBlock method="GET" path={`/api/conversations?agent_id=${id}`} />
          <ApiBlock method="GET" path="/api/conversations/{conversation_id}/messages" />
        </div>
      </Section>

      <Section title={t('apiRef.versioning')}>
        <div className="space-y-2">
          <ApiBlock method="GET" path={`/api/agents/${id}/versions`} />
          <ApiBlock method="POST" path={`/api/agents/${id}/versions/publish`} body='{ "version": 1, "description": "..." }' />
          <ApiBlock method="POST" path={`/api/agents/${id}/versions/{ver}/rollback`} />
          <ApiBlock method="GET" path={`/api/agents/${id}/versions/diff?v1=1&v2=2`} />
        </div>
      </Section>

      {agent.input_schema && (agent.input_schema as { variables?: unknown[] }).variables?.length ? (
        <Section title={t('apiRef.inputSchema')}>
          <pre className="text-xs bg-gray-50 dark:bg-gray-800 p-3 rounded-lg overflow-x-auto">
            {JSON.stringify(agent.input_schema, null, 2)}
          </pre>
        </Section>
      ) : null}

      <Section title={t('apiRef.sseEvents')}>
        <div className="grid grid-cols-2 sm:grid-cols-3 gap-1 text-xs">
          {[
            'graph_start', 'node_start', 'node_complete', 'node_skipped', 'node_error',
            'agent_token', 'agent_tool_call', 'agent_tool_result',
            'parallel_fork', 'parallel_join',
            'kanban_update', 'approval_required',
            'error', 'graph_end', 'done',
          ].map((ev) => (
            <span key={ev} className="px-2 py-1 bg-gray-50 dark:bg-gray-800 rounded font-mono">{ev}</span>
          ))}
        </div>
      </Section>
    </div>
  );
}
