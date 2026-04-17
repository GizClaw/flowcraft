import { describe, it, expect } from 'vitest';
import { render, screen } from '@testing-library/react';
import EventTimeline from './EventTimeline';
import type { ExecutionEvent } from '../../types/chat';

const events: ExecutionEvent[] = [
  { id: '1', type: 'graph_start', run_id: 'r1', node_id: '', created_at: new Date().toISOString() },
  { id: '2', type: 'node_start', run_id: 'r1', node_id: 'llm_1', created_at: new Date().toISOString() },
  { id: '3', type: 'node_complete', run_id: 'r1', node_id: 'llm_1', payload: { elapsed_ms: 150 }, created_at: new Date().toISOString() },
  { id: '4', type: 'node_error', run_id: 'r1', node_id: 'tool_1', payload: { error: 'timeout' }, created_at: new Date().toISOString() },
  { id: '5', type: 'graph_end', run_id: 'r1', node_id: '', created_at: new Date().toISOString() },
];

describe('EventTimeline', () => {
  it('renders all events', () => {
    render(<EventTimeline events={events} />);
    expect(screen.getByText('graph_start')).toBeInTheDocument();
    expect(screen.getByText('node_start')).toBeInTheDocument();
    expect(screen.getByText('node_complete')).toBeInTheDocument();
    expect(screen.getByText('node_error')).toBeInTheDocument();
    expect(screen.getByText('graph_end')).toBeInTheDocument();
  });

  it('renders node_id badges', () => {
    render(<EventTimeline events={events} />);
    expect(screen.getAllByText('llm_1')).toHaveLength(2);
    expect(screen.getByText('tool_1')).toBeInTheDocument();
  });

  it('renders elapsed_ms', () => {
    render(<EventTimeline events={events} />);
    expect(screen.getByText('150ms')).toBeInTheDocument();
  });

  it('renders error messages', () => {
    render(<EventTimeline events={events} />);
    expect(screen.getByText('timeout')).toBeInTheDocument();
  });

  it('renders empty list', () => {
    const { container } = render(<EventTimeline events={[]} />);
    expect(container.firstChild).toBeInTheDocument();
    expect(container.querySelectorAll('svg')).toHaveLength(0);
  });
});
