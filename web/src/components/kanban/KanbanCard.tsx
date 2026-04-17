import { useState, useCallback } from 'react';
import { User, Clock, AlertCircle, CheckCircle2, Loader2, FileText, Copy, Check } from 'lucide-react';
import Modal from '../common/Modal';
import MarkdownContent from '../message/MarkdownContent';
import type { KanbanCard as KanbanCardType } from '../../types/kanban';

const AGENT_COLORS = [
  'bg-violet-400', 'bg-cyan-400', 'bg-amber-400', 'bg-rose-400',
  'bg-emerald-400', 'bg-fuchsia-400', 'bg-sky-400', 'bg-orange-400',
];

const AGENT_DOT_COLORS = [
  'bg-violet-500', 'bg-cyan-500', 'bg-amber-500', 'bg-rose-500',
  'bg-emerald-500', 'bg-fuchsia-500', 'bg-sky-500', 'bg-orange-500',
];

function agentColorIndex(agentId: string): number {
  let hash = 0;
  for (let i = 0; i < agentId.length; i++) {
    hash = (hash * 31 + agentId.charCodeAt(i)) | 0;
  }
  return Math.abs(hash) % AGENT_COLORS.length;
}

const statusStyles: Record<string, { border: string; badge: string; badgeText: string; icon: React.ReactNode }> = {
  pending: {
    border: 'border-l-gray-400',
    badge: 'bg-gray-100 dark:bg-gray-700',
    badgeText: 'text-gray-600 dark:text-gray-300',
    icon: <Clock size={12} className="text-gray-400" />,
  },
  claimed: {
    border: 'border-l-blue-500',
    badge: 'bg-blue-50 dark:bg-blue-900/50',
    badgeText: 'text-blue-700 dark:text-blue-300',
    icon: <Loader2 size={12} className="text-blue-500 animate-spin" />,
  },
  done: {
    border: 'border-l-green-500',
    badge: 'bg-green-50 dark:bg-green-900/50',
    badgeText: 'text-green-700 dark:text-green-300',
    icon: <CheckCircle2 size={12} className="text-green-500" />,
  },
  failed: {
    border: 'border-l-red-500',
    badge: 'bg-red-50 dark:bg-red-900/50',
    badgeText: 'text-red-700 dark:text-red-300',
    icon: <AlertCircle size={12} className="text-red-500" />,
  },
};

function formatElapsed(ms?: number): string {
  if (!ms) return '';
  if (ms < 1000) return `${ms}ms`;
  if (ms < 60_000) return `${(ms / 1000).toFixed(1)}s`;
  return `${(ms / 60_000).toFixed(1)}m`;
}

function formatTime(iso: string): string {
  try {
    return new Date(iso).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', second: '2-digit' });
  } catch {
    return '';
  }
}

function CopyButton({ text }: { text: string }) {
  const [copied, setCopied] = useState(false);
  const handleCopy = useCallback(() => {
    navigator.clipboard.writeText(text).then(() => {
      setCopied(true);
      setTimeout(() => setCopied(false), 2000);
    });
  }, [text]);

  return (
    <button
      onClick={handleCopy}
      className="p-1 rounded hover:bg-gray-200 dark:hover:bg-gray-600 text-gray-400 hover:text-gray-600 dark:hover:text-gray-200 transition-colors"
      aria-label="Copy"
    >
      {copied ? <Check size={12} /> : <Copy size={12} />}
    </button>
  );
}

function DetailSection({ label, children, variant, copyText }: { label: string; children: React.ReactNode; variant?: 'error'; copyText?: string }) {
  return (
    <div>
      <div className="flex items-center justify-between mb-1.5">
        <p className={`text-[10px] font-semibold uppercase ${variant === 'error' ? 'text-red-400' : 'text-gray-400'}`}>{label}</p>
        {copyText && <CopyButton text={copyText} />}
      </div>
      {children}
    </div>
  );
}

export default function KanbanCard({ card }: { card: KanbanCardType }) {
  const style = statusStyles[card.status] || statusStyles.pending;
  const label = card.target_agent_id || card.template || card.type;
  const [detailOpen, setDetailOpen] = useState(false);

  return (
    <>
      <div
        onClick={() => setDetailOpen(true)}
        className={`bg-white dark:bg-gray-800 rounded-lg shadow-sm border-l-4 ${style.border} overflow-hidden cursor-pointer hover:shadow-md transition-shadow`}
      >
        <div className="p-3 space-y-2">
          {/* Header: type badge + elapsed */}
          <div className="flex items-center justify-between gap-2">
            <span className={`inline-flex items-center gap-1 text-[11px] font-medium px-1.5 py-0.5 rounded ${style.badge} ${style.badgeText}`}>
              {style.icon}
              {card.target_agent_id && (
                <span className={`w-2 h-2 rounded-full shrink-0 ${AGENT_DOT_COLORS[agentColorIndex(card.target_agent_id)]}`} />
              )}
              {label}
            </span>
            {card.elapsed_ms ? (
              <span className="text-[10px] text-gray-400 flex items-center gap-0.5 shrink-0">
                <Clock size={10} />
                {formatElapsed(card.elapsed_ms)}
              </span>
            ) : null}
          </div>

          {/* Query / description */}
          {card.query && (
            <p className="text-xs text-gray-700 dark:text-gray-300 line-clamp-3 leading-relaxed">{card.query}</p>
          )}

          {/* Output preview */}
          {card.output && (
            <div className="flex items-start gap-1.5 text-xs text-gray-500 dark:text-gray-400 bg-gray-50 dark:bg-gray-750 rounded p-1.5">
              <FileText size={11} className="shrink-0 mt-0.5" />
              <span className="line-clamp-2 leading-relaxed">{card.output}</span>
            </div>
          )}

          {/* Error */}
          {card.error && (
            <div className="flex items-start gap-1.5 text-xs text-red-600 dark:text-red-400 bg-red-50 dark:bg-red-900/30 rounded p-1.5">
              <AlertCircle size={11} className="shrink-0 mt-0.5" />
              <span className="line-clamp-2 leading-relaxed">{card.error}</span>
            </div>
          )}

          {/* Footer: agent + time */}
          <div className="flex items-center justify-between text-[10px] text-gray-400 pt-0.5">
            <div className="flex items-center gap-2">
              {card.consumer && card.consumer !== '*' && (
                <span className="flex items-center gap-1">
                  <User size={10} />
                  <span className="truncate max-w-[100px]">{card.consumer}</span>
                </span>
              )}
              {card.producer && (
                <span className="truncate max-w-[80px]">from {card.producer}</span>
              )}
            </div>
            <span>{formatTime(card.created_at)}</span>
          </div>
        </div>
      </div>

      <Modal open={detailOpen} onClose={() => setDetailOpen(false)} title={label} size="lg">
        <div className="space-y-4">
          {/* Status + meta */}
          <div className="flex items-center gap-3 flex-wrap">
            <span className={`inline-flex items-center gap-1 text-xs font-medium px-2 py-1 rounded ${style.badge} ${style.badgeText}`}>
              {style.icon}
              {card.status}
            </span>
            {card.elapsed_ms ? (
              <span className="text-xs text-gray-400 flex items-center gap-1">
                <Clock size={12} />
                {formatElapsed(card.elapsed_ms)}
              </span>
            ) : null}
            {card.consumer && card.consumer !== '*' && (
              <span className="text-xs text-gray-400 flex items-center gap-1">
                <User size={12} />
                {card.consumer}
              </span>
            )}
            <span className="text-xs text-gray-400">{formatTime(card.created_at)}</span>
          </div>

          {/* Query */}
          {card.query && (
            <DetailSection label="Input" copyText={card.query}>
              <div className="text-sm text-gray-700 dark:text-gray-300 bg-gray-50 dark:bg-gray-800 rounded-lg p-3 whitespace-pre-wrap break-words">
                {card.query}
              </div>
            </DetailSection>
          )}

          {/* Output */}
          {card.output && (
            <DetailSection label="Output" copyText={card.output}>
              <div className="text-sm bg-gray-50 dark:bg-gray-800 rounded-lg p-3 overflow-x-auto">
                <MarkdownContent content={card.output} />
              </div>
            </DetailSection>
          )}

          {/* Error */}
          {card.error && (
            <DetailSection label="Error" variant="error">
              <div className="text-sm text-red-600 dark:text-red-400 bg-red-50 dark:bg-red-900/30 rounded-lg p-3 whitespace-pre-wrap break-words">
                {card.error}
              </div>
            </DetailSection>
          )}
        </div>
      </Modal>
    </>
  );
}
