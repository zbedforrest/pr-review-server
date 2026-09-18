import { describe, expect, it } from 'vitest';
import type { PR } from '@/types/pr';
import { SIGN_IN_REASON, quickActionAvailability } from './quickActionPolicy';

const makePR = (partial: Partial<PR> = {}): PR => ({
  owner: 'acme',
  repo: 'example',
  number: 1,
  commit_sha: 'abc123',
  last_reviewed_at: null,
  review_html_path: '',
  github_url: '',
  review_url: '',
  status: 'completed',
  title: 'Example',
  author: 'bob',
  generating_since: null,
  approval_count: 0,
  my_review_status: '',
  draft: false,
  ci_state: 'success',
  ci_failed_checks: [],
  created_at: null,
  is_mine: false,
  via_teams: [],
  critical_count: 0,
  medium_count: 0,
  low_count: 0,
  notes: '',
  ...partial,
});

const enabledUser = { quick_actions_enabled: true, github_actions_available: true };

describe('quickActionAvailability', () => {
  it('enables everything for an open foreign PR with flag on and token present', () => {
    const a = quickActionAvailability(makePR(), enabledUser, true);
    expect(a.approve).toEqual({ enabled: true });
    expect(a.request_changes).toEqual({ enabled: true });
    expect(a.comment).toEqual({ enabled: true });
    expect(a.open).toEqual({ enabled: true });
    expect(a.copy).toEqual({ enabled: true });
  });

  it('disables approve and request changes on own PR', () => {
    const a = quickActionAvailability(makePR({ is_mine: true }), enabledUser, true);
    expect(a.approve).toEqual({ enabled: false, reason: 'You cannot approve your own PR' });
    expect(a.request_changes.enabled).toBe(false);
    expect(a.request_changes.reason).toMatch(/own PR/);
    expect(a.comment.enabled).toBe(true);
  });

  it('disables approve on a draft but allows request changes and comment', () => {
    const a = quickActionAvailability(makePR({ draft: true }), enabledUser, true);
    expect(a.approve.enabled).toBe(false);
    expect(a.approve.reason).toBe('Draft PR: approve needs PRism and Greptile green on this head (PRism and Greptile not green)');
    expect(a.request_changes.enabled).toBe(true);
    expect(a.comment.enabled).toBe(true);
  });

  it('allows approve on a draft when PRism and Greptile are both green on this head', () => {
    const a = quickActionAvailability(
      makePR({ draft: true, review_verdict: 'approve_suggestions', critical_count: 0, greptile_status: 'green' }),
      enabledUser,
      true
    );
    expect(a.approve).toEqual({ enabled: true });
  });

  it('names PRism when only Greptile is green on a draft', () => {
    const a = quickActionAvailability(
      makePR({ draft: true, review_verdict: 'request_changes', greptile_status: 'green' }),
      enabledUser,
      true
    );
    expect(a.approve.reason).toContain('(PRism not green)');
    const stale = quickActionAvailability(
      makePR({ draft: true, status: 'pending', review_verdict: 'approve', greptile_status: 'green' }),
      enabledUser,
      true
    );
    expect(stale.approve.reason).toContain('(PRism not green)');
    const critical = quickActionAvailability(
      makePR({ draft: true, review_verdict: 'approve', critical_count: 1, greptile_status: 'green' }),
      enabledUser,
      true
    );
    expect(critical.approve.reason).toContain('(PRism not green)');
  });

  it('names Greptile when only PRism is green on a draft', () => {
    for (const greptile_status of ['red', 'absent', undefined] as const) {
      const a = quickActionAvailability(
        makePR({ draft: true, review_verdict: 'approve', greptile_status }),
        enabledUser,
        true
      );
      expect(a.approve.reason).toContain('(Greptile not green)');
    }
  });

  it('disables all review actions on closed and merged PRs', () => {
    for (const pr_state of ['closed', 'merged'] as const) {
      const a = quickActionAvailability(makePR({ pr_state }), enabledUser, true);
      expect(a.approve).toEqual({ enabled: false, reason: `PR is ${pr_state}` });
      expect(a.request_changes.enabled).toBe(false);
      expect(a.comment.enabled).toBe(false);
      expect(a.open.enabled).toBe(true);
    }
  });

  it('treats an undefined pr_state as open', () => {
    expect(quickActionAvailability(makePR({ pr_state: undefined }), enabledUser, true).comment.enabled).toBe(true);
  });

  it('disables review actions and offers sign-in when token unavailable', () => {
    const a = quickActionAvailability(makePR(), { quick_actions_enabled: true, github_actions_available: false }, true);
    expect(a.approve).toEqual({ enabled: false, reason: SIGN_IN_REASON });
    expect(a.request_changes.reason).toBe(SIGN_IN_REASON);
    expect(a.comment.reason).toBe(SIGN_IN_REASON);
    expect(a.open.enabled).toBe(true);
  });

  it('disables review actions when the flag is off or the user is unknown', () => {
    expect(quickActionAvailability(makePR(), undefined, true).approve.enabled).toBe(false);
    expect(quickActionAvailability(makePR(), { quick_actions_enabled: false, github_actions_available: true }, true).comment.enabled).toBe(false);
  });

  it('disables copy link with a tooltip when the clipboard is unavailable', () => {
    expect(quickActionAvailability(makePR(), enabledUser, false).copy).toEqual({ enabled: false, reason: 'Clipboard unavailable' });
  });
});
