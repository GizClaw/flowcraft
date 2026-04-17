import { X, CheckCircle, AlertCircle, Info, AlertTriangle } from 'lucide-react';
import { useToastStore, type ToastType } from '../../store/toastStore';

const iconMap: Record<ToastType, typeof CheckCircle> = {
  success: CheckCircle,
  error: AlertCircle,
  info: Info,
  warning: AlertTriangle,
};

const colorMap: Record<ToastType, string> = {
  success: 'border-green-400 bg-green-50 dark:bg-green-950 text-green-800 dark:text-green-200',
  error: 'border-red-400 bg-red-50 dark:bg-red-950 text-red-800 dark:text-red-200',
  info: 'border-blue-400 bg-blue-50 dark:bg-blue-950 text-blue-800 dark:text-blue-200',
  warning: 'border-amber-400 bg-amber-50 dark:bg-amber-950 text-amber-800 dark:text-amber-200',
};

export default function ToastContainer() {
  const toasts = useToastStore((s) => s.toasts);
  const removeToast = useToastStore((s) => s.removeToast);

  if (toasts.length === 0) return null;

  return (
    <div className="fixed top-4 right-4 z-[100] flex flex-col gap-2 max-w-sm">
      {toasts.map((toast) => {
        const Icon = iconMap[toast.type];
        return (
          <div key={toast.id} className={`flex items-start gap-3 p-3 rounded-lg border shadow-lg ${colorMap[toast.type]} animate-in slide-in-from-right`}>
            <Icon size={18} className="mt-0.5 shrink-0" />
            <p className="text-sm flex-1">{toast.message}</p>
            <button onClick={() => removeToast(toast.id)} className="shrink-0 opacity-60 hover:opacity-100">
              <X size={16} />
            </button>
          </div>
        );
      })}
    </div>
  );
}
