import { useState, useEffect } from 'react';
import { useParams, useNavigate } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { ArrowLeft, Plus, Search, Trash2 } from 'lucide-react';
import { datasetApi } from '../utils/api';
import type { Dataset, DatasetDocument, QueryResult } from '../types/knowledge';
import Modal from '../components/common/Modal';
import LoadingSpinner from '../components/common/LoadingSpinner';
import { useToastStore } from '../store/toastStore';

export default function KnowledgeDetailPage() {
  const { t } = useTranslation();
  const { id } = useParams<{ id: string }>();
  const navigate = useNavigate();
  const [dataset, setDataset] = useState<Dataset | null>(null);
  const [docs, setDocs] = useState<DatasetDocument[]>([]);
  const [loading, setLoading] = useState(true);
  const [showAdd, setShowAdd] = useState(false);
  const [newTitle, setNewTitle] = useState('');
  const [newContent, setNewContent] = useState('');
  const [searchQuery, setSearchQuery] = useState('');
  const [maxLayer, setMaxLayer] = useState<0 | 1 | 2>(2);
  const [results, setResults] = useState<QueryResult[]>([]);
  const [searching, setSearching] = useState(false);
  const addToast = useToastStore((s) => s.addToast);

  const load = () => {
    if (!id) return;
    setLoading(true);
    Promise.all([datasetApi.get(id), datasetApi.listDocuments(id)])
      .then(([ds, d]) => { setDataset(ds); setDocs(d); })
      .catch(() => navigate('/knowledge'))
      .finally(() => setLoading(false));
  };

  useEffect(load, [id, navigate]);

  const handleAddDoc = async () => {
    if (!id || !newTitle.trim() || !newContent.trim()) return;
    try {
      await datasetApi.addDocument(id, { name: newTitle.trim(), content: newContent.trim() });
      setShowAdd(false); setNewTitle(''); setNewContent('');
      addToast('success', t('knowledge.docAdded'));
      load();
    } catch (err) { addToast('error', err instanceof Error ? err.message : t('knowledge.addFailed')); }
  };

  const handleSearch = async () => {
    if (!id || !searchQuery.trim()) return;
    setSearching(true);
    try {
      const r = await datasetApi.query(id, { query: searchQuery, top_k: 10, max_layer: maxLayer });
      setResults(r);
    } catch (err) { addToast('error', err instanceof Error ? err.message : t('knowledge.searchFailed')); }
    finally { setSearching(false); }
  };

  if (loading) return <div className="flex justify-center py-16"><LoadingSpinner /></div>;
  if (!dataset) return null;

  return (
    <div className="p-6 max-w-4xl mx-auto space-y-6">
      <div className="flex items-center gap-3">
        <button onClick={() => navigate('/knowledge')} className="p-1.5 rounded-lg hover:bg-gray-100 dark:hover:bg-gray-800 text-gray-500"><ArrowLeft size={18} /></button>
        <h1 className="text-xl font-bold text-gray-900 dark:text-gray-100">{dataset.name}</h1>
      </div>

      {/* Search */}
      <div className="bg-white dark:bg-gray-900 rounded-xl border border-gray-200 dark:border-gray-800 p-4">
        <h3 className="text-sm font-semibold text-gray-700 dark:text-gray-300 mb-3">{t('knowledge.search')}</h3>
        <div className="flex gap-2">
          <input value={searchQuery} onChange={(e) => setSearchQuery(e.target.value)} onKeyDown={(e) => e.key === 'Enter' && handleSearch()} placeholder={t('knowledge.searchPlaceholder')} className="flex-1 px-3 py-2 text-sm rounded-lg border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-800" />
          <select value={maxLayer} onChange={(e) => setMaxLayer(Number(e.target.value) as 0 | 1 | 2)} className="px-2 py-2 text-sm rounded-lg border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-800">
            <option value={0}>L0 (Abstract)</option>
            <option value={1}>L1 (Overview)</option>
            <option value={2}>L2 (Full)</option>
          </select>
          <button onClick={handleSearch} disabled={searching} className="flex items-center gap-1 px-4 py-2 bg-indigo-600 text-white rounded-lg hover:bg-indigo-700 disabled:opacity-50 text-sm">
            <Search size={14} /> {t('knowledge.search')}
          </button>
        </div>
        {results.length > 0 && (
          <div className="mt-3 space-y-2">
            {results.map((r, i) => (
              <div key={i} className="p-3 bg-gray-50 dark:bg-gray-800 rounded-lg">
                <div className="flex items-center gap-2 mb-1">
                  <span className="text-sm font-medium text-gray-700 dark:text-gray-300">{r.document_name}</span>
                  <span className="text-[10px] px-1.5 py-0.5 rounded bg-indigo-100 dark:bg-indigo-900 text-indigo-600">L{r.layer}</span>
                  <span className="text-[10px] text-gray-400">{t('knowledge.score')}: {r.score.toFixed(3)}</span>
                </div>
                <p className="text-xs text-gray-500 line-clamp-4">{r.content}</p>
              </div>
            ))}
          </div>
        )}
      </div>

      {/* Documents */}
      <div className="bg-white dark:bg-gray-900 rounded-xl border border-gray-200 dark:border-gray-800 p-4">
        <div className="flex items-center justify-between mb-3">
          <h3 className="text-sm font-semibold text-gray-700 dark:text-gray-300">{t('knowledge.documents')} ({docs.length})</h3>
          <button onClick={() => setShowAdd(true)} className="flex items-center gap-1 px-3 py-1.5 text-sm bg-indigo-600 text-white rounded-lg hover:bg-indigo-700">
            <Plus size={14} /> {t('knowledge.add')}
          </button>
        </div>
        <div className="space-y-2">
          {docs.map((doc) => (
            <div key={doc.id} className="flex items-center gap-3 p-3 bg-gray-50 dark:bg-gray-800 rounded-lg">
              <div className="flex-1 min-w-0">
                <p className="text-sm font-medium text-gray-700 dark:text-gray-300 truncate">{doc.name}</p>
                <div className="flex items-center gap-2 text-[10px] text-gray-400">
                  {doc.processing_status && <span className={`px-1 py-0.5 rounded ${doc.processing_status === 'completed' ? 'bg-green-100 dark:bg-green-900 text-green-600' : 'bg-amber-100 dark:bg-amber-900 text-amber-600'}`}>{doc.processing_status}</span>}
                  {doc.chunk_count && <span>{doc.chunk_count} chunks</span>}
                </div>
              </div>
              <button onClick={() => { if (id) datasetApi.deleteDocument(id, doc.id).then(load).catch(() => {}); }} className="p-1 text-red-400 hover:text-red-600">
                <Trash2 size={14} />
              </button>
            </div>
          ))}
        </div>
      </div>

      <Modal open={showAdd} onClose={() => setShowAdd(false)} title={t('knowledge.addDocument')} size="lg">
        <div className="space-y-3">
          <input value={newTitle} onChange={(e) => setNewTitle(e.target.value)} placeholder={t('knowledge.titleLabel')} className="w-full px-3 py-2 text-sm rounded-lg border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-800" autoFocus />
          <textarea value={newContent} onChange={(e) => setNewContent(e.target.value)} placeholder={t('knowledge.contentPlaceholder')} rows={10} className="w-full px-3 py-2 text-sm rounded-lg border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-800" />
          <button onClick={handleAddDoc} disabled={!newTitle.trim() || !newContent.trim()} className="w-full px-4 py-2 bg-indigo-600 text-white rounded-lg hover:bg-indigo-700 disabled:opacity-50 text-sm">{t('knowledge.addDocButton')}</button>
        </div>
      </Modal>
    </div>
  );
}
