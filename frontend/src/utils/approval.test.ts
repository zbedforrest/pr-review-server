import { describe, expect, it } from 'vitest';
import { approvalBucket, approvalScope, isApprovalCandidate, reconcileApprovalTarget, safeEvidenceURL } from './approval';
import type { PR } from '@/types/pr';
import type { ApprovalTarget } from '@/types/approval';
const sha = 'a'.repeat(40);
const pr = (overrides: Partial<PR> = {}): PR => ({ owner: 'acme', repo: 'example', number: 1, title: 'Example', author: 'alex', commit_sha: sha, via_teams: [], created_at: null, ...overrides } as PR);
const target = (overrides: Partial<ApprovalTarget> = {}): ApprovalTarget => ({ execution_status: 'completed', decision: 'candidate', freshness_state: 'current', valid_until: new Date(10000).toISOString(), ...overrides } as ApprovalTarget);
describe('approval scope', () => {
  it('deduplicates and excludes only known exact-head approvals', () => {
    expect(approvalScope([pr(), pr(), pr({ number: 2, my_review_status: 'APPROVED' }), pr({ number: 3, my_review_status: 'APPROVED', my_review_commit_sha: sha }), pr({ number: 4, my_review_status: 'APPROVED', my_review_commit_sha: 'b'.repeat(40) })], {}).map(p => p.number)).toEqual([1, 2, 4]);
  });
  it('respects global scope and excludes ineligible PRs', () => {
    const prs = [pr(), pr({ number: 2, draft: true }), pr({ number: 3, hidden: true }), pr({ number: 4, is_mine: true }), pr({ number: 5, pr_state: 'closed' }), pr({ number: 6, repo: 'other' })];
    expect(approvalScope(prs, { repos: ['acme/example'] }).map(p => p.number)).toEqual([1]);
  });
});
describe('candidate freshness', () => {
  it('removes expired and failed candidates from positive counts', () => {
    expect(isApprovalCandidate(target(), 9999)).toBe(true);
    expect(isApprovalCandidate(target(), 10000)).toBe(false);
    expect(isApprovalCandidate(target({ execution_status: 'failed' }), 100)).toBe(false);
    expect(approvalBucket(target(), 10000)).toBe('stale');
    expect(approvalBucket(target({ execution_status: 'failed' }), 10000)).toBe('failed');
  });
  it('rejects unsafe evidence destinations', () => {
    expect(safeEvidenceURL('javascript:alert(1)')).toBeUndefined();
    expect(safeEvidenceURL('https://github.com.evil.invalid/pull/1')).toBeUndefined();
    expect(safeEvidenceURL('https://github.com/acme/example/pull/1')).toBe('https://github.com/acme/example/pull/1');
  });
});

describe('observed dashboard changes', () => {
  it('immediately withdraws old-head and newly ineligible candidates', () => {
    const old = target({ revision: sha });
    for (const changed of [pr({ commit_sha: 'b'.repeat(40) }), pr({ draft: true }), pr({ pr_state: 'closed' }), pr({ hidden: true }), pr({ my_review_status: 'APPROVED', my_review_commit_sha: sha })]) {
      expect(isApprovalCandidate(reconcileApprovalTarget(old, changed), 100)).toBe(false);
    }
  });
  it('reconciles stale or replaced projections with cached evidence', () => {
    const cached = target({ target_id: 'old', revision: sha, assessment: { summary: 'Historical rationale' } as ApprovalTarget['assessment'] });
    const stale = reconcileApprovalTarget(cached, pr(), { ...cached, freshness_state: 'stale', reason_codes: ['evidence_changed'] });
    expect(isApprovalCandidate(stale, 100)).toBe(false);
    expect(stale.assessment?.summary).toBe('Historical rationale');
    const replaced = reconcileApprovalTarget(cached, pr(), { ...cached, target_id: 'new' });
    expect(replaced.freshness_state).toBe('stale');
    expect(replaced.reason_codes).toContain('superseded');
  });
});
