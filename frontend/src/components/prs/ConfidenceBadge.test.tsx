import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { ConfidenceBadge } from './ConfidenceBadge';

function renderBadge(score: number | null | undefined, size?: 'row' | 'large') {
  const { container } = render(<ConfidenceBadge score={score} size={size} />);
  return container.querySelector('.confidence-badge') as HTMLElement;
}

describe('ConfidenceBadge', () => {
  afterEach(() => {
    cleanup();
  });

  it.each([0, 3, 4, 5])('names score %i in its alt text and shows only the ratio in its tooltip', (score) => {
    const el = renderBadge(score);
    expect(el.getAttribute('role')).toBe('img');
    expect(el.getAttribute('aria-label')).toBe(`Merge confidence ${score}/5`);
    expect(el.querySelector('svg')).toBeTruthy();
    fireEvent.focus(el);
    expect(screen.getByRole('tooltip').textContent).toBe(`${score}/5`);
  });

  it.each([null, undefined, NaN])('renders the empty dash when the score is %s', (score) => {
    const el = renderBadge(score);
    expect(el.classList.contains('confidence-badge--empty')).toBe(true);
    expect(el.textContent).toBe('-');
    expect(el.getAttribute('role')).toBe('img');
    expect(el.getAttribute('aria-label')).toBe('No merge confidence yet');
    expect(el.querySelector('svg')).toBeNull();
    fireEvent.focus(el);
    expect(screen.getByRole('tooltip').textContent).toBe('-/5');
  });

  it('clamps out-of-range scores to the nearest medal', () => {
    expect(renderBadge(7).getAttribute('aria-label')).toBe('Merge confidence 5/5');
    cleanup();
    expect(renderBadge(-1).getAttribute('aria-label')).toBe('Merge confidence 0/5');
  });

  it('warns once per out-of-range value', () => {
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});
    renderBadge(9);
    cleanup();
    renderBadge(9);
    cleanup();
    renderBadge(4);
    expect(warn).toHaveBeenCalledTimes(1);
    expect(warn.mock.calls[0][0]).toContain('9');
    warn.mockRestore();
  });

  it('rounds fractional scores', () => {
    expect(renderBadge(3.6).getAttribute('aria-label')).toBe('Merge confidence 4/5');
  });

  it('defaults to the row size and accepts large', () => {
    expect(renderBadge(4).classList.contains('confidence-badge--row')).toBe(true);
    cleanup();
    const large = renderBadge(4, 'large');
    expect(large.classList.contains('confidence-badge--large')).toBe(true);
    expect(large.classList.contains('confidence-badge--row')).toBe(false);
  });
});
