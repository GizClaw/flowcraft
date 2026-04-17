import { useState, useEffect, useMemo } from 'react';
import { Trash2, Pencil, Save, X, Search } from 'lucide-react';
import { memoryApi, type MemoryEntry } from '../utils/api';
import { useToastStore } from '../store/toastStore';
import EmptyState from '../components/common/EmptyState';
import LoadingSpinner from '../components/common/LoadingSpinner';
import { formatDistanceToNow } from 'date-fns';

const CATEGORIES = ['profile', 'preferences', 'entities', 'events', 'cases', 'patterns'];

interface Props {
  agentId: string;
}

export default function AgentMemoryPage({ agentId }: Props) {
  const [category, setCategory] = useState('profile');
  const [entries, setEntries] = useState<MemoryEntry[]>([]);
  const [loading, setLoading] = useState(true);
  const [editingId, setEditingId] = useState<string | null>(null);
  const [editContent, setEditContent] = useState('');
  const [searchQuery, setSearchQuery] = useState('');
  const addToast = useToastStore((s) => s.addToast);

  const load = () => {
    setLoading(true);
    memoryApi.list(category).then(setEntries).catch(() => {}).finally(() => setLoading(false));
  };

  useEffect(load, [agentId, category]);

  const filtered = useMemo(() => {
    if (!searchQuery.trim()) return entries;
    const q = searchQuery.toLowerCase();
    return entries.filter((e) => e.content.toLowerCase().includes(q));
  }, [entries, searchQuery]);

  const handleDelete = async (entryId: string) => {
    try { await memoryApi.delete(entryId); load(); addToast('success', 'Memory deleted'); }
    catch (err) { addToast('error', err instanceof Error ? err.message : 'Delete failed'); }
  };

  const handleSaveEdit = async () => {
    if (!editingId) return;
    try {
      await memoryApi.update(editingId, editContent);
      setEditingId(null);
      load();
      addToast('success', 'Memory updated');
    } catch (err) {
      addToast('error', err instanceof Error ? err.message : 'Update failed');
    }
  };

  return (
    <div className="space-y-4">
      <div className="flex gap-1 flex-wrap">
        <div className="w-full text-xs text-gray-500 dark:text-gray-400">
          Shared user runtime memory. Current page is opened from agent `{agentId}` but data is user-scoped.
        </div>
        {CATEGORIES.map((cat) => (
          <button
            key={cat}
            onClick={() => setCategory(cat)}
            className={`px-3 py-1.5 text-sm rounded-lg capitalize ${category === cat ? 'bg-indigo-100 dark:bg-indigo-900 text-indigo-700 dark:text-indigo-300 font-medium' : 'text-gray-500 hover:bg-gray-100 dark:hover:bg-gray-800'}`}
          >
            {cat}
          </button>
        ))}
      </div>

      <div className="relative">
        <Search size={14} className="absolute left-3 top-1/2 -translate-y-1/2 text-gray-400" />
        <input
          value={searchQuery}
          onChange={(e) => setSearchQuery(e.target.value)}
          placeholder="Search memories..."
          className="w-full pl-9 pr-3 py-2 text-sm rounded-lg border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-800"
        />
      </div>

      {loading ? (
        <div className="flex justify-center py-8"><LoadingSpinner /></div>
      ) : filtered.length === 0 ? (
        <EmptyState title={`No ${category} memories`} description={searchQuery ? 'No memories match your search' : 'Long-term memories will accumulate as you use the agent'} />
      ) : (
        <div className="space-y-2">
          <p className="text-xs text-gray-500">{filtered.length} of {entries.length} entries</p>
          {filtered.map((entry) => (
            <div key={entry.id} className="p-3 bg-gray-50 dark:bg-gray-800 rounded-lg">
              {editingId === entry.id ? (
                <div className="space-y-2">
                  <textarea value={editContent} onChange={(e) => setEditContent(e.target.value)} rows={3} className="w-full px-3 py-2 text-sm rounded border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-700" />
                  <div className="flex gap-1">
                    <button onClick={handleSaveEdit} className="flex items-center gap-1 px-2 py-1 text-xs bg-indigo-600 text-white rounded"><Save size={12} /> Save</button>
                    <button onClick={() => setEditingId(null)} className="flex items-center gap-1 px-2 py-1 text-xs border border-gray-300 dark:border-gray-600 rounded"><X size={12} /> Cancel</button>
                  </div>
                </div>
              ) : (
                <div>
                  <div className="flex items-start gap-2">
                    <p className="flex-1 text-sm text-gray-700 dark:text-gray-300 whitespace-pre-wrap">{entry.content}</p>
                    <div className="flex gap-1 shrink-0">
                      <button onClick={() => { setEditingId(entry.id); setEditContent(entry.content); }} className="p-1 text-gray-400 hover:text-gray-600"><Pencil size={12} /></button>
                      <button onClick={() => handleDelete(entry.id)} className="p-1 text-red-400 hover:text-red-600"><Trash2 size={12} /></button>
                    </div>
                  </div>
                  <div className="flex gap-3 mt-1.5 text-[10px] text-gray-400">
                    {entry.source?.conversation_id && <span>Conversation: {entry.source.conversation_id.slice(0, 12)}…</span>}
                    <span>{formatDistanceToNow(new Date(entry.created_at), { addSuffix: true })}</span>
                  </div>
                </div>
              )}
            </div>
          ))}
        </div>
      )}
    </div>
  );
}
