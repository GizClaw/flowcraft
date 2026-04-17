import { useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Download, Upload } from 'lucide-react';
import Modal from '../common/Modal';
import { importExportApi } from '../../utils/api';
import { useToastStore } from '../../store/toastStore';

interface Props {
  agentId: string;
  onImportSuccess?: () => void;
}

export default function ImportExportDialog({ agentId, onImportSuccess }: Props) {
  const { t } = useTranslation();
  const [showExport, setShowExport] = useState(false);
  const [showImport, setShowImport] = useState(false);
  const [format, setFormat] = useState<'json' | 'yaml'>('json');
  const [importContent, setImportContent] = useState('');
  const [importErrors, setImportErrors] = useState<string[]>([]);
  const addToast = useToastStore((s) => s.addToast);

  const handleExport = async () => {
    try {
      const content = await importExportApi.exportGraph(agentId, format);
      const blob = new Blob([content], { type: format === 'yaml' ? 'text/yaml' : 'application/json' });
      const url = URL.createObjectURL(blob);
      const a = document.createElement('a');
      a.href = url;
      a.download = `workflow.${format}`;
      a.click();
      URL.revokeObjectURL(url);
      setShowExport(false);
      addToast('success', t('importExport.exported'));
    } catch (err) {
      addToast('error', err instanceof Error ? err.message : t('importExport.exportFailed'));
    }
  };

  const handleImport = async () => {
    try {
      const result = await importExportApi.importGraph(agentId, { format, content: importContent });
      if (result.errors && result.errors.length > 0) {
        setImportErrors(result.errors.map((e) => e.message));
      } else {
        setShowImport(false);
        setImportContent('');
        setImportErrors([]);
        addToast('success', t('importExport.imported'));
        onImportSuccess?.();
      }
    } catch (err) {
      addToast('error', err instanceof Error ? err.message : t('importExport.importFailed'));
    }
  };

  return (
    <>
      <div className="flex gap-1">
        <button onClick={() => setShowExport(true)} className="flex items-center gap-1 px-2 py-1 text-xs rounded border border-gray-300 dark:border-gray-600 hover:bg-gray-50 dark:hover:bg-gray-800">
          <Download size={12} /> {t('importExport.export')}
        </button>
        <button onClick={() => setShowImport(true)} className="flex items-center gap-1 px-2 py-1 text-xs rounded border border-gray-300 dark:border-gray-600 hover:bg-gray-50 dark:hover:bg-gray-800">
          <Upload size={12} /> {t('importExport.import')}
        </button>
      </div>

      <Modal open={showExport} onClose={() => setShowExport(false)} title={t('importExport.exportTitle')} size="sm">
        <div className="space-y-4">
          <div className="flex gap-2">
            {(['json', 'yaml'] as const).map((f) => (
              <button key={f} onClick={() => setFormat(f)} className={`px-3 py-1.5 text-sm rounded-lg ${format === f ? 'bg-indigo-600 text-white' : 'bg-gray-100 dark:bg-gray-800'}`}>{f.toUpperCase()}</button>
            ))}
          </div>
          <button onClick={handleExport} className="w-full px-4 py-2 bg-indigo-600 text-white rounded-lg hover:bg-indigo-700 text-sm">{t('importExport.download')}</button>
        </div>
      </Modal>

      <Modal open={showImport} onClose={() => setShowImport(false)} title={t('importExport.importTitle')} size="lg">
        <div className="space-y-4">
          <div className="flex gap-2">
            {(['json', 'yaml'] as const).map((f) => (
              <button key={f} onClick={() => setFormat(f)} className={`px-3 py-1.5 text-sm rounded-lg ${format === f ? 'bg-indigo-600 text-white' : 'bg-gray-100 dark:bg-gray-800'}`}>{f.toUpperCase()}</button>
            ))}
          </div>
          <textarea
            value={importContent}
            onChange={(e) => setImportContent(e.target.value)}
            placeholder={`Paste ${format.toUpperCase()} content...`}
            rows={12}
            className="w-full px-3 py-2 text-sm font-mono rounded-lg border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-800"
          />
          {importErrors.length > 0 && (
            <div className="space-y-1">
              {importErrors.map((e, i) => <p key={i} className="text-xs text-red-500">{e}</p>)}
            </div>
          )}
          <button onClick={handleImport} disabled={!importContent.trim()} className="w-full px-4 py-2 bg-indigo-600 text-white rounded-lg hover:bg-indigo-700 disabled:opacity-50 text-sm">{t('importExport.import')}</button>
        </div>
      </Modal>
    </>
  );
}
