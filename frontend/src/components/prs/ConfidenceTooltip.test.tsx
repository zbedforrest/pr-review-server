import { act, cleanup, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { ConfidenceBadge } from './ConfidenceBadge';

const badge = () => document.querySelector('.confidence-badge') as HTMLElement;
const tooltip = () => screen.queryByRole('tooltip');

describe('ConfidenceBadge hover tooltip', () => {
  beforeEach(() => vi.useFakeTimers());
  afterEach(() => {
    cleanup();
    vi.useRealTimers();
  });

  it('opens about half a second after the pointer arrives, not immediately', () => {
    render(<ConfidenceBadge score={4} />);
    fireEvent.mouseEnter(badge());
    act(() => vi.advanceTimersByTime(200));
    expect(tooltip()).toBeNull();
    act(() => vi.advanceTimersByTime(400));
    expect(tooltip()).toBeTruthy();
    expect(tooltip()!.textContent).toContain('Merge confidence 4/5: Merge Ascendant');
    expect(tooltip()!.textContent).toContain('Minor findings worth a look before merge.');
    expect(tooltip()!.textContent).toContain('a request-changes verdict caps the score at 3');
  });

  it('never opens when the pointer leaves before the delay', () => {
    render(<ConfidenceBadge score={4} />);
    fireEvent.mouseEnter(badge());
    act(() => vi.advanceTimersByTime(200));
    fireEvent.mouseLeave(badge());
    act(() => vi.advanceTimersByTime(1000));
    expect(tooltip()).toBeNull();
  });

  it('closes when the pointer leaves and on Escape', () => {
    render(<ConfidenceBadge score={2} />);
    fireEvent.mouseEnter(badge());
    act(() => vi.advanceTimersByTime(600));
    expect(tooltip()).toBeTruthy();
    fireEvent.mouseLeave(badge());
    expect(tooltip()).toBeNull();

    fireEvent.mouseEnter(badge());
    act(() => vi.advanceTimersByTime(600));
    fireEvent.keyDown(document, { key: 'Escape' });
    expect(tooltip()).toBeNull();
  });

  it('opens on keyboard focus without a delay and describes the badge', () => {
    render(<ConfidenceBadge score={5} />);
    fireEvent.focus(badge());
    expect(tooltip()).toBeTruthy();
    expect(badge().getAttribute('aria-describedby')).toBe(tooltip()!.id);
    fireEvent.blur(badge());
    expect(tooltip()).toBeNull();
  });

  it('does not use the browser title tooltip', () => {
    render(<ConfidenceBadge score={1} />);
    expect(badge().hasAttribute('title')).toBe(false);
  });

  it('explains the dash the same way', () => {
    render(<ConfidenceBadge score={null} />);
    fireEvent.mouseEnter(badge());
    act(() => vi.advanceTimersByTime(600));
    expect(tooltip()!.textContent).toContain('The score appears when the next review of this PR completes.');
  });
});
