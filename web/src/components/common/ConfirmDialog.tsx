import { useTranslation } from 'react-i18next';
import Modal from './Modal';

interface Props {
  open: boolean;
  onClose: () => void;
  onConfirm: () => void;
  title: string;
  message: string;
  confirmLabel?: string;
  variant?: 'danger' | 'default';
}

export default function ConfirmDialog({ open, onClose, onConfirm, title, message, confirmLabel, variant = 'default' }: Props) {
  const { t } = useTranslation();
  const btnClass = variant === 'danger'
    ? 'bg-red-600 hover:bg-red-700 text-white'
    : 'bg-indigo-600 hover:bg-indigo-700 text-white';

  return (
    <Modal open={open} onClose={onClose} title={title} size="sm">
      <p className="text-sm text-gray-600 dark:text-gray-400 mb-6">{message}</p>
      <div className="flex justify-end gap-3">
        <button onClick={onClose} className="px-4 py-2 text-sm rounded-lg border border-gray-300 dark:border-gray-600 hover:bg-gray-50 dark:hover:bg-gray-800">
          {t('confirm.cancel')}
        </button>
        <button onClick={() => { onConfirm(); onClose(); }} className={`px-4 py-2 text-sm rounded-lg ${btnClass}`}>
          {confirmLabel || t('common.confirm')}
        </button>
      </div>
    </Modal>
  );
}
