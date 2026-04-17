import { useState } from 'react';
import { Upload, AlertTriangle } from 'lucide-react';
import Modal from '../common/Modal';
import { versionApi, type GraphVersion, type GraphDiff } from '../../utils/api';
import { useToastStore } from '../../store/toastStore';
import { useWorkflowStore } from '../../store/workflowStore';

interface Props {
  agentId: string;
  currentVersion?: GraphVersion;
  latestPublishedVersion?: GraphVersion;
  onPublished: () => void;
}

export default function PublishDialog({ agentId, currentVersion, latestPublishedVersion, onPublished }: Props) {
  const [open, setOpen] = useState(false);
  const [diff, setDiff] = useState<GraphDiff | null>(null);
  const [loading, setLoading] = useState(false);
  const [publishing, setPublishing] = useState(false);
  const addToast = useToastStore((s) => s.addToast);
  const isDirty = useWorkflowStore((s) => s.isDirty);

  const handleOpen = async () => {
    setOpen(true);
    if (currentVersion && latestPublishedVersion) {
      setLoading(true);
      try {
        const d = await versionApi.diff(agentId, latestPublishedVersion.version, currentVersion.version);
        setDiff(d);
      } catch { setDiff(null); }
      setLoading(false);
    }
  };

  const handlePublish = async () => {
    setPublishing(true);
    try {
      if (!currentVersion) {
        addToast('error', 'No draft version to publish');
        return;
      }
      await versionApi.publish(agentId, currentVersion.version);
      addToast('success', 'Version published');
      setOpen(false);
      onPublished();
    } catch (err) {
      const msg = err instanceof Error ? err.message : 'Publish failed';
      if (msg.includes('no changes since')) {
        addToast('warning', msg);
      } else {
        addToast('error', msg);
      }
    } finally {
      setPublishing(false);
    }
  };

  const totalChanges = diff
    ? (diff.nodes_added?.length ?? 0) + (diff.nodes_removed?.length ?? 0) + (diff.nodes_changed?.length ?? 0) + (diff.edges_added?.length ?? 0) + (diff.edges_removed?.length ?? 0)
    : 0;

  return (
    <>
      <button onClick={handleOpen} className="flex items-center gap-1 px-3 py-1.5 text-sm bg-indigo-600 text-white rounded-lg hover:bg-indigo-700">
        <Upload size={14} /> Publish
      </button>
      <Modal open={open} onClose={() => setOpen(false)} title="Publish Version">
        <div className="space-y-4">
          {currentVersion && (
            <div className="p-3 bg-gray-50 dark:bg-gray-800 rounded-lg">
              <p className="text-sm text-gray-700 dark:text-gray-300">
                Current draft: <span className="font-medium">v{currentVersion.version}</span>
              </p>
              {currentVersion.checksum && (
                <p className="text-xs text-gray-400 mt-1 font-mono">Checksum: {currentVersion.checksum}</p>
              )}
            </div>
          )}

          {loading ? (
            <p className="text-sm text-gray-500 text-center py-4">Loading diff...</p>
          ) : diff && totalChanges > 0 ? (
            <div className="space-y-2">
              <p className="text-xs font-medium text-gray-500 uppercase">Changes Summary</p>
              <div className="grid grid-cols-3 gap-2 text-sm">
                {(diff.nodes_added?.length ?? 0) > 0 && (
                  <span className="text-green-600">+{diff.nodes_added!.length} node(s)</span>
                )}
                {(diff.nodes_removed?.length ?? 0) > 0 && (
                  <span className="text-red-600">-{diff.nodes_removed!.length} node(s)</span>
                )}
                {(diff.nodes_changed?.length ?? 0) > 0 && (
                  <span className="text-amber-600">~{diff.nodes_changed!.length} modified</span>
                )}
                {(diff.edges_added?.length ?? 0) > 0 && (
                  <span className="text-green-600">+{diff.edges_added!.length} edge(s)</span>
                )}
                {(diff.edges_removed?.length ?? 0) > 0 && (
                  <span className="text-red-600">-{diff.edges_removed!.length} edge(s)</span>
                )}
              </div>
            </div>
          ) : (
            <p className="text-sm text-gray-500 text-center py-2">No previous version to compare</p>
          )}

          {isDirty && (
            <div className="flex items-start gap-2 p-3 bg-amber-50 dark:bg-amber-950/40 border border-amber-200 dark:border-amber-800 rounded-lg">
              <AlertTriangle size={16} className="text-amber-500 mt-0.5 shrink-0" />
              <p className="text-sm text-amber-700 dark:text-amber-300">
                You have unsaved changes in the editor. Please save before publishing to ensure your latest edits are included.
              </p>
            </div>
          )}

          <div className="flex gap-2 justify-end">
            <button onClick={() => setOpen(false)} className="px-4 py-2 text-sm border border-gray-300 dark:border-gray-600 rounded-lg hover:bg-gray-50 dark:hover:bg-gray-800">
              Cancel
            </button>
            <button onClick={handlePublish} disabled={publishing || isDirty} className="px-4 py-2 text-sm bg-indigo-600 text-white rounded-lg hover:bg-indigo-700 disabled:opacity-50">
              {publishing ? 'Publishing...' : 'Confirm Publish'}
            </button>
          </div>
        </div>
      </Modal>
    </>
  );
}
