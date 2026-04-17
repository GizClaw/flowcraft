import { describe, it, expect, vi } from 'vitest';
import { render, screen, fireEvent } from '@testing-library/react';
import ConfirmDialog from './ConfirmDialog';

vi.mock('react-i18next', () => ({
  useTranslation: () => ({
    t: (key: string) => {
      const map: Record<string, string> = {
        'confirm.cancel': 'Cancel',
        'common.confirm': 'Confirm',
      };
      return map[key] || key;
    },
  }),
}));

describe('ConfirmDialog', () => {
  const baseProps = {
    open: true,
    onClose: vi.fn(),
    onConfirm: vi.fn(),
    title: 'Delete item?',
    message: 'This action cannot be undone.',
  };

  it('renders title and message', () => {
    render(<ConfirmDialog {...baseProps} />);
    expect(screen.getByText('Delete item?')).toBeInTheDocument();
    expect(screen.getByText('This action cannot be undone.')).toBeInTheDocument();
  });

  it('renders cancel and confirm buttons', () => {
    render(<ConfirmDialog {...baseProps} />);
    expect(screen.getByText('Cancel')).toBeInTheDocument();
    expect(screen.getByText('Confirm')).toBeInTheDocument();
  });

  it('uses custom confirm label', () => {
    render(<ConfirmDialog {...baseProps} confirmLabel="Delete" />);
    expect(screen.getByText('Delete')).toBeInTheDocument();
  });

  it('calls onConfirm and onClose when confirm clicked', () => {
    const onConfirm = vi.fn();
    const onClose = vi.fn();
    render(<ConfirmDialog {...baseProps} onConfirm={onConfirm} onClose={onClose} />);
    fireEvent.click(screen.getByText('Confirm'));
    expect(onConfirm).toHaveBeenCalledTimes(1);
    expect(onClose).toHaveBeenCalledTimes(1);
  });

  it('calls onClose when cancel clicked', () => {
    const onClose = vi.fn();
    render(<ConfirmDialog {...baseProps} onClose={onClose} />);
    fireEvent.click(screen.getByText('Cancel'));
    expect(onClose).toHaveBeenCalledTimes(1);
  });

  it('applies danger variant styling', () => {
    render(<ConfirmDialog {...baseProps} variant="danger" confirmLabel="Delete" />);
    const btn = screen.getByText('Delete');
    expect(btn.className).toContain('bg-red-600');
  });
});
