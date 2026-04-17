import { describe, it, expect } from 'vitest';
import { render, screen, fireEvent } from '@testing-library/react';
import ToolCallCard from './ToolCallCard';
import type { ToolCallInfo } from '../../types/chat';

const baseTc: ToolCallInfo = {
  name: 'sandbox_bash',
  args: '{"cmd":"ls -la"}',
  status: 'success',
  result: 'file1.txt\nfile2.txt',
};

describe('ToolCallCard', () => {
  it('renders tool name', () => {
    render(<ToolCallCard tc={baseTc} />);
    expect(screen.getByText('sandbox_bash')).toBeInTheDocument();
  });

  it('shows success icon for completed tool call', () => {
    const { container } = render(<ToolCallCard tc={baseTc} />);
    const svgs = container.querySelectorAll('svg');
    const successSvg = Array.from(svgs).find(svg =>
      (svg.className.baseVal || svg.getAttribute('class') || '').includes('text-emerald-500')
    );
    expect(successSvg).toBeTruthy();
  });

  it('shows error icon for failed tool call', () => {
    const { container } = render(<ToolCallCard tc={{ ...baseTc, status: 'error' }} />);
    const svgs = container.querySelectorAll('svg');
    const errorSvg = Array.from(svgs).find(svg =>
      (svg.className.baseVal || svg.getAttribute('class') || '').includes('text-red-500')
    );
    expect(errorSvg).toBeTruthy();
  });

  it('shows spinner for pending tool call', () => {
    const { container } = render(<ToolCallCard tc={{ ...baseTc, status: 'pending', result: undefined }} />);
    const svgs = container.querySelectorAll('svg');
    const spinner = Array.from(svgs).find(svg =>
      (svg.className.baseVal || svg.getAttribute('class') || '').includes('animate-spin')
    );
    expect(spinner).toBeTruthy();
  });

  it('expands to show arguments on click', () => {
    render(<ToolCallCard tc={baseTc} />);
    expect(screen.queryByText('Arguments')).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole('button'));
    expect(screen.getByText('Arguments')).toBeInTheDocument();
  });

  it('shows result when expanded', () => {
    render(<ToolCallCard tc={baseTc} />);
    fireEvent.click(screen.getByRole('button'));
    expect(screen.getByText('Result')).toBeInTheDocument();
  });

  it('shows Running text for pending without result', () => {
    render(<ToolCallCard tc={{ ...baseTc, status: 'pending', result: undefined }} />);
    fireEvent.click(screen.getByRole('button'));
    expect(screen.getByText('Running...')).toBeInTheDocument();
  });

  it('collapses on second click', () => {
    render(<ToolCallCard tc={baseTc} />);
    const btn = screen.getByRole('button');
    fireEvent.click(btn);
    expect(screen.getByText('Arguments')).toBeInTheDocument();
    fireEvent.click(btn);
    expect(screen.queryByText('Arguments')).not.toBeInTheDocument();
  });
});
