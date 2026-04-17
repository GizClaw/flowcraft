import { describe, it, expect, beforeEach } from 'vitest';
import { render, screen, fireEvent } from '@testing-library/react';
import { useToastStore } from '../../store/toastStore';
import ToastContainer from './ToastContainer';

beforeEach(() => {
  useToastStore.setState({ toasts: [] });
});

describe('ToastContainer', () => {
  it('renders nothing when no toasts', () => {
    const { container } = render(<ToastContainer />);
    expect(container.firstChild).toBeNull();
  });

  it('renders toast messages', () => {
    useToastStore.setState({
      toasts: [
        { id: '1', type: 'success', message: 'Saved successfully' },
        { id: '2', type: 'error', message: 'Something went wrong' },
      ],
    });
    render(<ToastContainer />);
    expect(screen.getByText('Saved successfully')).toBeInTheDocument();
    expect(screen.getByText('Something went wrong')).toBeInTheDocument();
  });

  it('removes toast when dismiss button clicked', () => {
    useToastStore.setState({
      toasts: [{ id: 't1', type: 'info', message: 'Hello' }],
    });
    render(<ToastContainer />);
    expect(screen.getByText('Hello')).toBeInTheDocument();
    const buttons = screen.getAllByRole('button');
    fireEvent.click(buttons[0]);
    expect(useToastStore.getState().toasts).toHaveLength(0);
  });
});
