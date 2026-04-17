import type React from 'react';
import { useState, useCallback } from 'react';
import ReactMarkdown from 'react-markdown';
import remarkGfm from 'remark-gfm';
import { Prism as SyntaxHighlighter } from 'react-syntax-highlighter';
import { oneDark } from 'react-syntax-highlighter/dist/esm/styles/prism';
import { Check, Copy } from 'lucide-react';

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
      className="absolute top-2 right-2 p-1 rounded bg-gray-700/60 hover:bg-gray-600 text-gray-300 hover:text-white transition-colors"
      aria-label="Copy code"
    >
      {copied ? <Check size={14} /> : <Copy size={14} />}
    </button>
  );
}

const tableWrapper = {
  table({ children }: { children?: React.ReactNode }) {
    return (
      <div className="overflow-x-auto -mx-1 my-2">
        <table className="min-w-full text-xs border-collapse">{children}</table>
      </div>
    );
  },
  th({ children }: { children?: React.ReactNode }) {
    return <th className="px-2 py-1.5 text-left font-semibold border-b border-gray-300 dark:border-gray-600 whitespace-nowrap bg-gray-50 dark:bg-gray-700/50">{children}</th>;
  },
  td({ children }: { children?: React.ReactNode }) {
    return <td className="px-2 py-1.5 border-b border-gray-200 dark:border-gray-700 whitespace-nowrap">{children}</td>;
  },
};

const fullComponents: import('react-markdown').Components = {
  ...tableWrapper,
  code({ className, children, ...props }) {
    const match = /language-(\w+)/.exec(className || '');
    const code = String(children).replace(/\n$/, '');
    if (match) {
      return (
        <div className="relative group my-1">
          <CopyButton text={code} />
          <SyntaxHighlighter style={oneDark} language={match[1]} PreTag="div" customStyle={{ margin: 0, borderRadius: '0.5rem', fontSize: '0.8rem' }}>
            {code}
          </SyntaxHighlighter>
        </div>
      );
    }
    return <code className="bg-gray-200 dark:bg-gray-700 px-1 py-0.5 rounded text-[0.8rem] break-all" {...props}>{children}</code>;
  },
};

const streamComponents: import('react-markdown').Components = {
  ...tableWrapper,
  code({ className, children, ...props }) {
    const match = /language-(\w+)/.exec(className || '');
    const code = String(children).replace(/\n$/, '');
    if (match) {
      return (
        <div className="relative group my-1">
          <CopyButton text={code} />
          <pre className="bg-gray-900 text-gray-100 p-3 rounded-lg overflow-x-auto text-[0.8rem]">
            <code>{children}</code>
          </pre>
        </div>
      );
    }
    return <code className="bg-gray-200 dark:bg-gray-700 px-1 py-0.5 rounded text-[0.8rem] break-all" {...props}>{children}</code>;
  },
};

interface Props {
  content: string;
  streaming?: boolean;
}

export default function MarkdownContent({ content, streaming }: Props) {
  return (
    <div className="prose prose-sm dark:prose-invert max-w-none break-words [overflow-wrap:anywhere]">
      <ReactMarkdown remarkPlugins={[remarkGfm]} components={streaming ? streamComponents : fullComponents}>
        {streaming ? content + '▊' : content}
      </ReactMarkdown>
    </div>
  );
}
