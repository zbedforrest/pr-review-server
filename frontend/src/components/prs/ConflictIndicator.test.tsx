import { cleanup, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it } from 'vitest';
import { ConflictIndicator } from './ConflictIndicator';

type IndicatorPR = Parameters<typeof ConflictIndicator>[0]['pr'];

const makePR = (partial: Partial<IndicatorPR> = {}): IndicatorPR => ({
  merge_state_status: 'DIRTY',
  pr_state: 'open',
  draft: false,
  hidden: false,
  ...partial,
});

describe('ConflictIndicator', () => {
  afterEach(() => cleanup());

  it('marks an open PR whose merge state is DIRTY', () => {
    render(<ConflictIndicator pr={makePR()} />);
    const el = screen.getByRole('img', { name: 'Merge conflicts with the base branch' });
    expect(el.getAttribute('title')).toBe(el.getAttribute('aria-label'));
  });

  it('marks a draft with conflicts too', () => {
    render(<ConflictIndicator pr={makePR({ draft: true })} />);
    expect(screen.getByRole('img', { name: /Merge conflicts/ })).toBeTruthy();
  });

  it.each(['CLEAN', 'BLOCKED', 'BEHIND', 'UNKNOWN', '', undefined] as const)('renders nothing for merge state %s', (state) => {
    const { container } = render(<ConflictIndicator pr={makePR({ merge_state_status: state })} />);
    expect(container.innerHTML).toBe('');
  });

  it('renders nothing for closed, merged or hidden rows', () => {
    const { container, rerender } = render(<ConflictIndicator pr={makePR({ pr_state: 'merged' })} />);
    expect(container.innerHTML).toBe('');
    rerender(<ConflictIndicator pr={makePR({ pr_state: 'closed' })} />);
    expect(container.innerHTML).toBe('');
    rerender(<ConflictIndicator pr={makePR({ hidden: true })} />);
    expect(container.innerHTML).toBe('');
  });
});
