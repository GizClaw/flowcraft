import { Loader2 } from 'lucide-react';

export default function LoadingSpinner({ size = 24, className = '' }: { size?: number; className?: string }) {
  return <Loader2 size={size} className={`animate-spin text-indigo-500 ${className}`} />;
}
