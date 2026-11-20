import { act, cleanup, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { IconTooltip } from './IconTooltip';

const renderIcon = () => {
  render(
    <IconTooltip label="Merge conflicts" className="icon">
      ⚔️
    </IconTooltip>
  );
  return screen.getByRole('img', { name: 'Merge conflicts' });
};

describe('IconTooltip', () => {
  beforeEach(() => vi.useFakeTimers());
  afterEach(() => {
    cleanup();
    vi.useRealTimers();
  });

  it('shows the label 300ms after the pointer arrives', () => {
    const icon = renderIcon();
    fireEvent.mouseEnter(icon);
    act(() => vi.advanceTimersByTime(299));
    expect(screen.queryByRole('tooltip')).toBeNull();
    act(() => vi.advanceTimersByTime(1));
    expect(screen.getByRole('tooltip').textContent).toBe('Merge conflicts');
  });

  it('leaves the browser title unset so only one tooltip appears', () => {
    expect(renderIcon().hasAttribute('title')).toBe(false);
  });

  it('never shows when the pointer leaves before the delay', () => {
    const icon = renderIcon();
    fireEvent.mouseEnter(icon);
    act(() => vi.advanceTimersByTime(200));
    fireEvent.mouseLeave(icon);
    act(() => vi.advanceTimersByTime(500));
    expect(screen.queryByRole('tooltip')).toBeNull();
  });

  it('shows immediately on keyboard focus and closes on Escape', () => {
    const icon = renderIcon();
    fireEvent.focus(icon);
    expect(screen.getByRole('tooltip')).toBeTruthy();
    fireEvent.keyDown(document, { key: 'Escape' });
    expect(screen.queryByRole('tooltip')).toBeNull();
  });
});
