import { useState } from 'react';
import { CheckCircle, XCircle } from 'lucide-react';

interface Props {
  prompt?: string;
  onDecision: (decision: 'approved' | 'rejected', comment?: string) => void;
  disabled?: boolean;
}

export default function ApprovalPanel({ prompt, onDecision, disabled }: Props) {
  const [comment, setComment] = useState('');
  const [submitting, setSubmitting] = useState(false);

  const handleDecision = (decision: 'approved' | 'rejected') => {
    setSubmitting(true);
    onDecision(decision, comment || undefined);
  };

  const isDisabled = disabled || submitting;

  return (
    <div className="p-4 bg-amber-50 dark:bg-amber-950 border border-amber-200 dark:border-amber-800 rounded-lg space-y-3">
      <h3 className="text-sm font-semibold text-amber-800 dark:text-amber-200">Approval Required</h3>
      {prompt && <p className="text-sm text-amber-700 dark:text-amber-300">{prompt}</p>}
      <textarea
        value={comment}
        onChange={(e) => setComment(e.target.value)}
        placeholder="Optional comment..."
        rows={2}
        className="w-full px-3 py-2 text-sm rounded-lg border border-amber-300 dark:border-amber-700 bg-white dark:bg-gray-800"
      />
      <div className="flex gap-2">
        <button
          onClick={() => handleDecision('approved')}
          disabled={isDisabled}
          className="flex items-center gap-1 px-4 py-2 bg-green-600 text-white rounded-lg hover:bg-green-700 disabled:opacity-50 text-sm"
        >
          <CheckCircle size={14} /> Approve
        </button>
        <button
          onClick={() => handleDecision('rejected')}
          disabled={isDisabled}
          className="flex items-center gap-1 px-4 py-2 bg-red-600 text-white rounded-lg hover:bg-red-700 disabled:opacity-50 text-sm"
        >
          <XCircle size={14} /> Reject
        </button>
      </div>
    </div>
  );
}
