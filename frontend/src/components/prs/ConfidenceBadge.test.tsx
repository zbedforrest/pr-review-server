import { cleanup, render } from '@testing-library/react';
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

  it.each([
    [0, 'Merge confidence 0/5: Merge Meltdown', 'Merge Meltdown: This diff needs a cleanup crew, not a rubber stamp.'],
    [3, 'Merge confidence 3/5: Diff Defender', 'Diff Defender: Holding the line. Still checking the exits.'],
    [5, 'Merge confidence 5/5: Merge Majesty', 'Merge Majesty: No blockers. Let the merge button wear the crown.'],
  ])('labels score %i with its medal name and tagline', (score, ariaLabel, title) => {
    const el = renderBadge(score);
    expect(el.getAttribute('role')).toBe('img');
    expect(el.getAttribute('aria-label')).toBe(ariaLabel);
    expect(el.getAttribute('title')).toBe(title);
    expect(el.querySelector('svg')).toBeTruthy();
  });

  it.each([null, undefined])('renders the empty dash when the score is %s', (score) => {
    const el = renderBadge(score);
    expect(el.classList.contains('confidence-badge--empty')).toBe(true);
    expect(el.textContent).toBe('-');
    expect(el.getAttribute('role')).toBe('img');
    expect(el.getAttribute('aria-label')).toBe('No merge confidence yet');
    expect(el.getAttribute('title')).toBe('No merge confidence yet');
    expect(el.querySelector('svg')).toBeNull();
  });

  it('clamps out-of-range scores to the nearest medal', () => {
    expect(renderBadge(7).getAttribute('aria-label')).toBe('Merge confidence 5/5: Merge Majesty');
    cleanup();
    expect(renderBadge(-1).getAttribute('aria-label')).toBe('Merge confidence 0/5: Merge Meltdown');
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
    expect(renderBadge(3.6).getAttribute('aria-label')).toBe('Merge confidence 4/5: Merge Ascendant');
  });

  it('defaults to the row size and accepts large', () => {
    expect(renderBadge(4).classList.contains('confidence-badge--row')).toBe(true);
    cleanup();
    const large = renderBadge(4, 'large');
    expect(large.classList.contains('confidence-badge--large')).toBe(true);
    expect(large.classList.contains('confidence-badge--row')).toBe(false);
  });
});
