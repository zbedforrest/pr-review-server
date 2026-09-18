import { cleanup, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it } from 'vitest';
import { MergeReadyIndicator } from './MergeReadyIndicator';

type IndicatorPR = Parameters<typeof MergeReadyIndicator>[0]['pr'];

const makePR = (partial: Partial<IndicatorPR> = {}): IndicatorPR => ({
  ready_to_merge: true,
  approval_count: 2,
  review_decision: '',
  merge_state_status: 'CLEAN',
  pr_state: 'open',
  draft: false,
  hidden: false,
  ...partial,
});

describe('MergeReadyIndicator', () => {
  afterEach(() => cleanup());

  it('renders the check with an accessible label when ready_to_merge is true', () => {
    render(<MergeReadyIndicator pr={makePR()} />);
    const el = screen.getByRole('img', { name: /^Ready to merge:/ });
    expect(el.textContent?.trim()).toBe('✅');
    expect(el.getAttribute('title')).toBe(el.getAttribute('aria-label'));
    expect(el.className).toBe('pr-table__merge-ready');
  });

  it('renders nothing when ready_to_merge is false or absent', () => {
    const { container, rerender } = render(<MergeReadyIndicator pr={makePR({ ready_to_merge: false })} />);
    expect(container.innerHTML).toBe('');
    rerender(<MergeReadyIndicator pr={makePR({ ready_to_merge: undefined })} />);
    expect(container.innerHTML).toBe('');
  });

  it('renders nothing for hidden rows even if the server says ready', () => {
    const { container } = render(<MergeReadyIndicator pr={makePR({ hidden: true })} />);
    expect(container.innerHTML).toBe('');
  });

  it('renders nothing for merged or closed rows', () => {
    const { container, rerender } = render(<MergeReadyIndicator pr={makePR({ pr_state: 'merged' })} />);
    expect(container.innerHTML).toBe('');
    rerender(<MergeReadyIndicator pr={makePR({ pr_state: 'closed' })} />);
    expect(container.innerHTML).toBe('');
  });

  it('renders nothing for drafts', () => {
    const { container } = render(<MergeReadyIndicator pr={makePR({ draft: true })} />);
    expect(container.innerHTML).toBe('');
  });

  it('pluralises approvals in the tooltip', () => {
    const { rerender } = render(<MergeReadyIndicator pr={makePR({ approval_count: 1 })} />);
    expect(screen.getByRole('img').getAttribute('aria-label')).toContain('1 approval,');
    rerender(<MergeReadyIndicator pr={makePR({ approval_count: 2 })} />);
    expect(screen.getByRole('img').getAttribute('aria-label')).toContain('2 approvals,');
    rerender(<MergeReadyIndicator pr={makePR({ approval_count: 0 })} />);
    expect(screen.getByRole('img').getAttribute('aria-label')).toContain('0 approvals,');
  });

  it('describes required reviews as approved when review_decision is APPROVED', () => {
    const { rerender } = render(<MergeReadyIndicator pr={makePR({ review_decision: 'APPROVED' })} />);
    expect(screen.getByRole('img').getAttribute('aria-label')).toContain('required reviews approved');
    rerender(<MergeReadyIndicator pr={makePR({ review_decision: '' })} />);
    expect(screen.getByRole('img').getAttribute('aria-label')).toContain('no required reviews outstanding');
  });
});
