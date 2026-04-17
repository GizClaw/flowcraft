import { useState, useEffect } from 'react';
import { Plus, Minus, Pencil } from 'lucide-react';
import { versionApi, type GraphDiff } from '../../utils/api';
import LoadingSpinner from '../common/LoadingSpinner';

interface Props {
  agentId: string;
  v1: number;
  v2: number;
}

export default function VersionDiff({ agentId, v1, v2 }: Props) {
  const [diff, setDiff] = useState<GraphDiff | null>(null);
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    setLoading(true);
    versionApi.diff(agentId, v1, v2).then(setDiff).catch(() => {}).finally(() => setLoading(false));
  }, [agentId, v1, v2]);

  if (loading) return <div className="flex justify-center p-8"><LoadingSpinner /></div>;
  if (!diff) return <p className="text-sm text-gray-500 text-center p-4">Failed to load diff</p>;

  const nodesAdded = diff.nodes_added ?? [];
  const nodesRemoved = diff.nodes_removed ?? [];
  const nodesChanged = diff.nodes_changed ?? [];
  const edgesAdded = diff.edges_added ?? [];
  const edgesRemoved = diff.edges_removed ?? [];

  const isEmpty = !nodesAdded.length && !nodesRemoved.length && !nodesChanged.length && !edgesAdded.length && !edgesRemoved.length;

  if (isEmpty) return <p className="text-sm text-gray-500 text-center p-4">No differences between v{v1} and v{v2}</p>;

  return (
    <div className="space-y-4 p-4">
      <h4 className="text-sm font-semibold text-gray-700 dark:text-gray-300">v{v1} → v{v2}</h4>

      {nodesAdded.length > 0 && (
        <Section title="Added Nodes" icon={<Plus size={14} className="text-green-500" />}>
          {nodesAdded.map((n) => <DiffItem key={n.id} label={`${n.id} (${n.type})`} color="green" />)}
        </Section>
      )}

      {nodesRemoved.length > 0 && (
        <Section title="Removed Nodes" icon={<Minus size={14} className="text-red-500" />}>
          {nodesRemoved.map((n) => <DiffItem key={n.id} label={`${n.id} (${n.type})`} color="red" />)}
        </Section>
      )}

      {nodesChanged.length > 0 && (
        <Section title="Modified Nodes" icon={<Pencil size={14} className="text-amber-500" />}>
          {nodesChanged.map((n) => (
            <div key={n.node_id} className="text-xs p-2 bg-amber-50 dark:bg-amber-950 rounded">
              <span className="font-medium text-amber-700 dark:text-amber-300">{n.node_id}</span>
              <pre className="mt-1 text-gray-500 text-[10px] overflow-x-auto">{JSON.stringify(n.after, null, 2)}</pre>
            </div>
          ))}
        </Section>
      )}

      {edgesAdded.length > 0 && (
        <Section title="Added Edges" icon={<Plus size={14} className="text-green-500" />}>
          {edgesAdded.map((e) => <DiffItem key={`${e.from}-${e.to}`} label={`${e.from} → ${e.to}`} color="green" />)}
        </Section>
      )}

      {edgesRemoved.length > 0 && (
        <Section title="Removed Edges" icon={<Minus size={14} className="text-red-500" />}>
          {edgesRemoved.map((e) => <DiffItem key={`${e.from}-${e.to}`} label={`${e.from} → ${e.to}`} color="red" />)}
        </Section>
      )}
    </div>
  );
}

function Section({ title, icon, children }: { title: string; icon: React.ReactNode; children: React.ReactNode }) {
  return (
    <div>
      <div className="flex items-center gap-1 mb-1">{icon}<span className="text-xs font-medium text-gray-500">{title}</span></div>
      <div className="space-y-1">{children}</div>
    </div>
  );
}

function DiffItem({ label, color }: { label: string; color: 'green' | 'red' }) {
  const bg = color === 'green' ? 'bg-green-50 dark:bg-green-950 text-green-700 dark:text-green-300' : 'bg-red-50 dark:bg-red-950 text-red-700 dark:text-red-300';
  return <div className={`text-xs p-1.5 rounded ${bg}`}>{label}</div>;
}
