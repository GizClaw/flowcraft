import { useState, useEffect } from 'react';
import { useNavigate } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { Plus, BookOpen, Trash2 } from 'lucide-react';
import { datasetApi } from '../utils/api';
import type { Dataset, CreateDatasetRequest } from '../types/knowledge';
import Modal from '../components/common/Modal';
import EmptyState from '../components/common/EmptyState';
import LoadingSpinner from '../components/common/LoadingSpinner';
import { useToastStore } from '../store/toastStore';
import { formatDistanceToNow } from 'date-fns';

export default function KnowledgePage() {
  const { t } = useTranslation();
  const [datasets, setDatasets] = useState<Dataset[]>([]);
  const [loading, setLoading] = useState(true);
  const [showCreate, setShowCreate] = useState(false);
  const [newName, setNewName] = useState('');
  const [newDesc, setNewDesc] = useState('');
  const navigate = useNavigate();
  const addToast = useToastStore((s) => s.addToast);

  const load = () => {
    setLoading(true);
    datasetApi.list().then(setDatasets).catch(() => {}).finally(() => setLoading(false));
  };

  useEffect(load, []);

  const handleCreate = async () => {
    if (!newName.trim()) return;
    try {
      await datasetApi.create({ name: newName.trim(), description: newDesc.trim() || undefined } as CreateDatasetRequest);
      setShowCreate(false); setNewName(''); setNewDesc('');
      addToast('success', t('knowledge.datasetCreated'));
      load();
    } catch (err) {
      addToast('error', err instanceof Error ? err.message : t('knowledge.createFailed'));
    }
  };

  const handleDelete = async (id: string) => {
    try { await datasetApi.delete(id); load(); addToast('success', t('knowledge.datasetDeleted')); }
    catch (err) { addToast('error', err instanceof Error ? err.message : t('knowledge.deleteFailed')); }
  };

  if (loading) return <div className="flex justify-center py-16"><LoadingSpinner /></div>;

  return (
    <div className="p-6">
      <div className="flex items-center justify-between mb-6">
        <h1 className="text-2xl font-bold text-gray-900 dark:text-gray-100">{t('knowledge.title')}</h1>
        <button onClick={() => setShowCreate(true)} className="flex items-center gap-2 px-4 py-2 bg-indigo-600 text-white rounded-lg hover:bg-indigo-700 text-sm">
          <Plus size={16} /> {t('knowledge.newDataset')}
        </button>
      </div>

      {datasets.length === 0 ? (
        <EmptyState title={t('knowledge.noDatasets')} description={t('knowledge.noDatasetsDesc')} />
      ) : (
        <div className="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-3 gap-4">
          {datasets.map((ds) => (
            <div key={ds.id} onClick={() => navigate(`/knowledge/${ds.id}`)} className="bg-white dark:bg-gray-900 rounded-xl border border-gray-200 dark:border-gray-800 p-4 hover:shadow-lg cursor-pointer transition-shadow group">
              <div className="flex items-start justify-between">
                <div className="flex items-center gap-2 mb-2">
                  <BookOpen size={16} className="text-cyan-500" />
                  <h3 className="text-sm font-semibold text-gray-900 dark:text-gray-100">{ds.name}</h3>
                </div>
                <button onClick={(e) => { e.stopPropagation(); handleDelete(ds.id); }} className="p-1 opacity-0 group-hover:opacity-100 text-red-400 hover:text-red-600">
                  <Trash2 size={14} />
                </button>
              </div>
              {ds.description && <p className="text-xs text-gray-500 mb-2 line-clamp-2">{ds.description}</p>}
              {ds.l0_abstract && <p className="text-xs text-gray-400 italic mb-2 line-clamp-2">{ds.l0_abstract}</p>}
              <div className="flex items-center gap-2 text-[10px] text-gray-400">
                <span>{ds.document_count ?? 0} {t('knowledge.docs')}</span>
                <span>{formatDistanceToNow(new Date(ds.updated_at), { addSuffix: true })}</span>
              </div>
            </div>
          ))}
        </div>
      )}

      <Modal open={showCreate} onClose={() => setShowCreate(false)} title={t('knowledge.createDataset')} size="sm">
        <div className="space-y-3">
          <input value={newName} onChange={(e) => setNewName(e.target.value)} placeholder={t('knowledge.datasetName')} className="w-full px-3 py-2 text-sm rounded-lg border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-800" autoFocus />
          <input value={newDesc} onChange={(e) => setNewDesc(e.target.value)} placeholder={t('knowledge.descriptionOptional')} className="w-full px-3 py-2 text-sm rounded-lg border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-800" />
          <button onClick={handleCreate} disabled={!newName.trim()} className="w-full px-4 py-2 bg-indigo-600 text-white rounded-lg hover:bg-indigo-700 disabled:opacity-50 text-sm">{t('knowledge.create')}</button>
        </div>
      </Modal>
    </div>
  );
}
