import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import type { PR } from '@/types/pr';
import { PRTable } from './PRTable';

vi.mock('./PRTableRow', () => ({
  PRTableRow: ({ pr }: { pr: PR }) => (
    <tr data-testid="row">
      <td>{pr.number}</td>
    </tr>
  ),
}));

const makePR = (partial: Partial<PR> = {}): PR => ({
  owner: 'test-org',
  repo: 'test-repo',
  number: 1,
  commit_sha: 'abc123',
  last_reviewed_at: null,
  review_html_path: '',
  github_url: 'https://github.com/test-org/test-repo/pull/1',
  review_url: '',
  status: 'pending',
  title: 'Example PR',
  author: 'alice',
  generating_since: null,
  approval_count: 0,
  my_review_status: '',
  draft: false,
  ci_state: 'unknown',
  ci_failed_checks: [],
  created_at: '2026-04-15T12:00:00Z',
  is_mine: false,
  via_teams: [],
  critical_count: 0,
  medium_count: 0,
  low_count: 0,
  notes: '',
  ...partial,
});

const MEDAL_NAMES = [
  'Merge Majesty',
  'Merge Ascendant',
  'Diff Defender',
  'Patch Gauntlet',
  'Rollback Reckoning',
  'Merge Meltdown',
];

const headerTexts = () => screen.getAllByRole('columnheader').map((th) => th.textContent?.trim());
const legendButton = () => screen.getByRole('button', { name: 'Confidence' });

describe('PRTable header', () => {
  afterEach(() => cleanup());

  it('places the Confidence column immediately before AI Review', () => {
    render(<PRTable prs={[makePR()]} />);
    const headers = headerTexts();
    const idx = headers.indexOf('Confidence');
    expect(idx).toBeGreaterThan(0);
    expect(headers[idx + 1]).toBe('AI Review');
    expect(headers).toContain('Via Teams');
  });

  it('keeps the Confidence column before AI Review when Via Teams is hidden', () => {
    render(<PRTable prs={[makePR()]} showViaTeams={false} />);
    const headers = headerTexts();
    expect(headers).not.toContain('Via Teams');
    expect(headers[headers.indexOf('Confidence') + 1]).toBe('AI Review');
  });

  it('renders the Confidence header as a closed legend trigger', () => {
    render(<PRTable prs={[makePR()]} />);
    const button = legendButton();
    expect(button.getAttribute('aria-haspopup')).toBe('dialog');
    expect(button.getAttribute('aria-expanded')).toBe('false');
    expect(screen.queryByRole('dialog')).toBeNull();
  });

  it('opens a legend listing all six medals with their rank words and the scoring rule', () => {
    render(<PRTable prs={[makePR()]} />);
    fireEvent.click(legendButton());
    expect(legendButton().getAttribute('aria-expanded')).toBe('true');
    const dialog = screen.getByRole('dialog');
    for (const name of MEDAL_NAMES) {
      expect(dialog.textContent).toContain(name);
    }
    expect(dialog.querySelectorAll('.confidence-badge--large')).toHaveLength(6);
    expect(dialog.textContent).toContain('Crowned');
    expect(dialog.textContent).toContain('Wreck');
  });

  it('describes the presence-based scoring rule', () => {
    render(<PRTable prs={[makePR()]} />);
    fireEvent.click(legendButton());
    const rule = screen.getByRole('dialog').querySelector('.confidence-legend__rule')?.textContent;
    expect(rule).toBe(
      'Starts at 5 with no blocking findings. Any critical finding costs 2; any medium costs 1, and three or more mediums cost 1 more; a violated required check costs 1; a request-changes verdict caps the score at 3.'
    );
  });

  it('caps the legend height at the viewport room below the header so it scrolls instead of clipping', () => {
    render(<PRTable prs={[makePR()]} />);
    fireEvent.click(legendButton());
    const dialog = screen.getByRole('dialog');
    expect(dialog.style.maxHeight).toBe(`${window.innerHeight - 6 - 8}px`);
  });

  it('toggles the legend closed on a second click and on Escape', () => {
    render(<PRTable prs={[makePR()]} />);
    fireEvent.click(legendButton());
    fireEvent.click(legendButton());
    expect(screen.queryByRole('dialog')).toBeNull();
    fireEvent.click(legendButton());
    expect(screen.getByRole('dialog')).toBeTruthy();
    fireEvent.keyDown(document, { key: 'Escape' });
    expect(screen.queryByRole('dialog')).toBeNull();
    expect(legendButton().getAttribute('aria-expanded')).toBe('false');
  });

  it('moves focus into the legend on open and back to the trigger on Escape', () => {
    render(<PRTable prs={[makePR()]} />);
    fireEvent.click(legendButton());
    expect(document.activeElement).toBe(screen.getByRole('dialog'));
    fireEvent.keyDown(document, { key: 'Escape' });
    expect(document.activeElement).toBe(legendButton());
  });

  it('does not steal focus on mount', () => {
    render(<PRTable prs={[makePR()]} />);
    expect(document.activeElement).toBe(document.body);
  });

  it('closes when focus leaves the legend and its trigger, and leaves focus where it went', () => {
    render(
      <>
        <PRTable prs={[makePR()]} />
        <a href="#next">next</a>
      </>
    );
    const next = screen.getByRole('link', { name: 'next' });
    fireEvent.click(legendButton());
    fireEvent.blur(screen.getByRole('dialog'), { relatedTarget: legendButton() });
    expect(screen.getByRole('dialog')).toBeTruthy();
    fireEvent.blur(legendButton(), { relatedTarget: screen.getByRole('dialog') });
    expect(screen.getByRole('dialog')).toBeTruthy();
    next.focus();
    fireEvent.blur(screen.getByRole('dialog'), { relatedTarget: next });
    expect(screen.queryByRole('dialog')).toBeNull();
    expect(document.activeElement).toBe(next);
  });

  it('renders the empty state without a table', () => {
    render(<PRTable prs={[]} />);
    expect(screen.getByText('No PRs found.')).toBeTruthy();
    expect(screen.queryByRole('table')).toBeNull();
  });
});
