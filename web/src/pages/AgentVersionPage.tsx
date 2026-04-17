import { useState } from 'react';
import { useOutletContext } from 'react-router-dom';
import VersionList from '../components/versioning/VersionList';
import VersionDiff from '../components/versioning/VersionDiff';
import type { Agent } from '../types/app';

export default function AgentVersionPage() {
  const { agent } = useOutletContext<{ agent: Agent }>();
  const [diffVersions, setDiffVersions] = useState<{ v1: number; v2: number } | null>(null);

  return (
    <div className="p-6 max-w-3xl mx-auto space-y-6">
      <VersionList agentId={agent.id} onSelectDiff={(v1, v2) => setDiffVersions({ v1, v2 })} />
      {diffVersions && (
        <div className="bg-white dark:bg-gray-900 rounded-xl border border-gray-200 dark:border-gray-800">
          <VersionDiff agentId={agent.id} v1={diffVersions.v1} v2={diffVersions.v2} />
        </div>
      )}
    </div>
  );
}
