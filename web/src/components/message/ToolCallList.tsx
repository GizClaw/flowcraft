import ToolCallCard from './ToolCallCard';
import type { ToolCallInfo } from '../../types/chat';

export default function ToolCallList({ toolCalls }: { toolCalls: ToolCallInfo[] }) {
  if (toolCalls.length === 0) return null;
  return (
    <div className="space-y-1.5">
      {toolCalls.map((tc, i) => <ToolCallCard key={tc.id || i} tc={tc} />)}
    </div>
  );
}
