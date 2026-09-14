import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { ConfidenceBadge } from './ConfidenceBadge';
import { SCORING_RULE } from './confidenceCopy';

function renderBadge(score: number | null | undefined, size?: 'row' | 'large') {
  const { container } = render(<ConfidenceBadge score={score} size={size} />);
  return container.querySelector('.confidence-badge') as HTMLElement;
}

describe('ConfidenceBadge', () => {
  afterEach(() => {
    cleanup();
  });

  it.each([
    [0, 'Merge confidence 0/5, Merge Meltdown. Significant findings; please address before merge.',
      ['Merge confidence 0/5: Merge Meltdown', 'Significant findings; please address before merge.', 'This diff needs a cleanup crew, not a rubber stamp.', SCORING_RULE]],
    [3, 'Merge confidence 3/5, Diff Defender. Findings that should be addressed before merge.',
      ['Merge confidence 3/5: Diff Defender', 'Findings that should be addressed before merge.', 'Holding the line. Still checking the exits.', SCORING_RULE]],
    [4, 'Merge confidence 4/5, Merge Ascendant. Minor findings worth a look before merge.',
      ['Merge confidence 4/5: Merge Ascendant', 'Minor findings worth a look before merge.', 'One last patrol before the victory lap.', SCORING_RULE]],
    [5, 'Merge confidence 5/5, Merge Majesty. No blocking findings.',
      ['Merge confidence 5/5: Merge Majesty', 'No blocking findings.', 'No blockers. Let the merge button wear the crown.', SCORING_RULE]],
  ])('explains score %i in its alt text and tooltip', (score, ariaLabel, tooltipLines) => {
    const el = renderBadge(score);
    expect(el.getAttribute('role')).toBe('img');
    expect(el.getAttribute('aria-label')).toBe(ariaLabel);
    expect(el.querySelector('svg')).toBeTruthy();
    fireEvent.focus(el);
    const lines = [...screen.getByRole('tooltip').querySelectorAll('p')].map((p) => p.textContent);
    expect(lines).toEqual(tooltipLines);
  });

  it.each([null, undefined, NaN])('renders the empty dash when the score is %s', (score) => {
    const el = renderBadge(score);
    expect(el.classList.contains('confidence-badge--empty')).toBe(true);
    expect(el.textContent).toBe('-');
    expect(el.getAttribute('role')).toBe('img');
    expect(el.getAttribute('aria-label')).toBe('No merge confidence yet');
    expect(el.querySelector('svg')).toBeNull();
    fireEvent.focus(el);
    expect(screen.getByRole('tooltip').textContent).toContain('The score appears when the next review of this PR completes.');
  });

  it('clamps out-of-range scores to the nearest medal', () => {
    expect(renderBadge(7).getAttribute('aria-label')).toContain('Merge confidence 5/5, Merge Majesty');
    cleanup();
    expect(renderBadge(-1).getAttribute('aria-label')).toContain('Merge confidence 0/5, Merge Meltdown');
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
    expect(renderBadge(3.6).getAttribute('aria-label')).toContain('Merge confidence 4/5, Merge Ascendant');
  });

  it('defaults to the row size and accepts large', () => {
    expect(renderBadge(4).classList.contains('confidence-badge--row')).toBe(true);
    cleanup();
    const large = renderBadge(4, 'large');
    expect(large.classList.contains('confidence-badge--large')).toBe(true);
    expect(large.classList.contains('confidence-badge--row')).toBe(false);
  });
});
