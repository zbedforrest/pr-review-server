import { act, cleanup, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { ToastHost } from './Toast';
import { TOAST_DURATION_MS, toast } from './toastStore';

describe('ToastHost', () => {
  beforeEach(() => {
    vi.useFakeTimers();
  });
  afterEach(() => {
    toast.clearAll();
    cleanup();
    vi.useRealTimers();
  });

  it('renders and auto-dismisses', () => {
    render(<ToastHost />);
    act(() => {
      toast.info({ title: 'Commented on acme/example #1 as @alice' });
    });
    expect(screen.getByText('Commented on acme/example #1 as @alice')).toBeTruthy();
    expect(screen.getByRole('status').getAttribute('aria-live')).toBe('polite');
    act(() => {
      vi.advanceTimersByTime(TOAST_DURATION_MS - 1);
    });
    expect(screen.queryByText(/Commented on/)).toBeTruthy();
    act(() => {
      vi.advanceTimersByTime(1);
    });
    expect(screen.queryByText(/Commented on/)).toBeNull();
  });

  it('success toast links to GitHub', () => {
    render(<ToastHost />);
    act(() => {
      toast.success({ title: 'Approved acme/example #1 as @alice', href: 'https://github.com/acme/example/pull/1#pullrequestreview-7' });
    });
    const link = screen.getByRole('link', { name: /View on GitHub/ });
    expect(link.getAttribute('href')).toBe('https://github.com/acme/example/pull/1#pullrequestreview-7');
    expect(link.getAttribute('target')).toBe('_blank');
    expect(link.closest('.toast')?.className).toContain('toast--success');
  });

  it('stacks toasts and dismisses one on click', () => {
    render(<ToastHost />);
    act(() => {
      toast.success({ title: 'first' });
      toast.error({ title: 'second' });
    });
    expect(screen.getAllByText(/first|second/)).toHaveLength(2);
    expect(screen.getByText('second').closest('.toast')?.className).toContain('toast--danger');
    fireEvent.click(screen.getByText('first'));
    expect(screen.queryByText('first')).toBeNull();
    expect(screen.getByText('second')).toBeTruthy();
  });
});
