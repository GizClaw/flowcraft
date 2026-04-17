import { useState, useCallback } from 'react';
import { CheckCircle, XCircle, Loader2, Wrench, Copy, Check } from 'lucide-react';
import Modal from '../common/Modal';
import MarkdownContent from './MarkdownContent';
import type { ToolCallInfo } from '../../types/chat';

function formatArgs(args: string): string {
  try { return JSON.stringify(JSON.parse(args), null, 2); } catch { return args; }
}

function isJSON(s: string): boolean {
  try { JSON.parse(s); return true; } catch { return false; }
}

function formatJSON(s: string): string {
  try { return JSON.stringify(JSON.parse(s), null, 2); } catch { return s; }
}

function summarizeArgs(args: string, max = 40): string {
  if (!args) return '';
  try {
    const obj = JSON.parse(args);
    const joined = Object.values(obj).map((v) => typeof v === 'string' ? v : JSON.stringify(v)).join(', ');
    return joined.length > max ? joined.slice(0, max) + '...' : joined;
  } catch {
    return args.length > max ? args.slice(0, max) + '...' : args;
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

function DetailSection({ label, children, copyText }: { label: string; children: React.ReactNode; copyText?: string }) {
  return (
    <div>
      <div className="flex items-center justify-between mb-1.5">
        <p className="text-[10px] font-semibold text-gray-400 uppercase">{label}</p>
        {copyText && <CopyButton text={copyText} />}
      </div>
      {children}
    </div>
  );
}

export default function ToolCallCard({ tc }: { tc: ToolCallInfo }) {
  const [detailOpen, setDetailOpen] = useState(false);

  const statusIcon = tc.status === 'pending'
    ? <Loader2 size={12} className="animate-spin text-blue-500" />
    : tc.status === 'success'
      ? <CheckCircle size={12} className="text-emerald-500" />
      : <XCircle size={12} className="text-red-500" />;

  const statusColor = tc.status === 'pending'
    ? 'border-blue-200 dark:border-blue-800 bg-blue-50/50 dark:bg-blue-950/30'
    : tc.status === 'success'
      ? 'border-emerald-200 dark:border-emerald-800 bg-emerald-50/50 dark:bg-emerald-950/30'
      : 'border-red-200 dark:border-red-800 bg-red-50/50 dark:bg-red-950/30';

  const argsSummary = summarizeArgs(tc.args);

  return (
    <>
      <button
        type="button"
        onClick={() => setDetailOpen((open) => !open)}
        aria-expanded={detailOpen}
        className={`w-full text-left border rounded-lg overflow-hidden cursor-pointer hover:shadow-sm transition-shadow ${statusColor}`}
      >
        <div className="flex items-center gap-2 px-2.5 py-1.5 min-w-0">
          <Wrench size={11} className="text-gray-400 shrink-0" />
          <span className="font-mono text-xs text-gray-700 dark:text-gray-300 shrink-0">{tc.name}</span>
          {argsSummary && <span className="text-[11px] text-gray-400 truncate min-w-0">{argsSummary}</span>}
          <span className="shrink-0 ml-auto">{statusIcon}</span>
        </div>
      </button>

      <Modal open={detailOpen} onClose={() => setDetailOpen(false)} title={tc.name} size="lg">
        <div className="space-y-4">
          <div className="flex items-center gap-2">
            {statusIcon}
            <span className="text-xs text-gray-500 capitalize">{tc.status}</span>
          </div>

          {tc.args && (
            <DetailSection label="Arguments">
              <pre className="text-xs bg-gray-900 text-gray-100 p-3 rounded-lg overflow-x-auto whitespace-pre-wrap break-words">
                {formatArgs(tc.args)}
              </pre>
            </DetailSection>
          )}

          {tc.result != null && (
            <DetailSection label="Result" copyText={tc.result}>
              {isJSON(tc.result) ? (
                <pre className="text-xs bg-gray-900 text-gray-100 p-3 rounded-lg overflow-x-auto whitespace-pre-wrap break-words">
                  {formatJSON(tc.result)}
                </pre>
              ) : (
                <div className="text-sm bg-gray-50 dark:bg-gray-800 rounded-lg p-3 overflow-x-auto">
                  <MarkdownContent content={tc.result} />
                </div>
              )}
            </DetailSection>
          )}

          {tc.status === 'pending' && !tc.result && (
            <p className="text-sm text-gray-400 italic">Running...</p>
          )}
        </div>
      </Modal>
    </>
  );
}
