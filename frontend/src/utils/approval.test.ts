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
  it('respects global scope, excludes ineligible PRs and investigates drafts', () => {
    const prs = [pr(), pr({ number: 2, draft: true }), pr({ number: 3, hidden: true }), pr({ number: 4, is_mine: true }), pr({ number: 5, pr_state: 'closed' }), pr({ number: 6, repo: 'other' })];
    expect(approvalScope(prs, { repos: ['acme/example'] }).map(p => p.number)).toEqual([1, 2]);
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
    for (const changed of [pr({ commit_sha: 'b'.repeat(40) }), pr({ draft: true }), pr({ pr_state: 'closed' }), pr({ hidden: true }), pr({ my_review_status: 'APPROVED', my_review_commit_sha: sha }), pr({ ci_state: 'failure' }), pr({ ci_state: 'pending' }), pr({ review_decision: 'CHANGES_REQUESTED' }), pr({ status: 'agent_reviewing' })]) {
      expect(isApprovalCandidate(reconcileApprovalTarget(old, changed), 100)).toBe(false);
    }
  });
  it('keeps a non-candidate decided on the condition it observes', () => {
    const cases: [string, Partial<PR>][] = [['pr_draft', { draft: true }], ['ci_failed', { ci_state: 'failure' }], ['ci_pending', { ci_state: 'pending' }], ['human_changes_requested', { review_decision: 'CHANGES_REQUESTED' }], ['review_in_progress', { status: 'agent_reviewing' }]];
    for (const [code, observed] of cases) {
      const decided = target({ revision: sha, decision: 'needs_attention', reason_codes: [code] });
      expect(reconcileApprovalTarget(decided, pr(observed)).freshness_state).toBe('current');
    }
  });
  it('still marks a non-candidate stale for a condition it was not decided on', () => {
    const draft = target({ revision: sha, decision: 'insufficient_evidence', reason_codes: ['pr_draft'] });
    const failing = reconcileApprovalTarget(draft, pr({ draft: true, ci_state: 'failure' }));
    expect(failing.freshness_state).toBe('stale');
    expect(failing.reason_codes).toContain('ci_failed');
    expect(reconcileApprovalTarget(draft, pr({ draft: true, commit_sha: 'b'.repeat(40) })).reason_codes).toContain('head_changed');
  });
  it('withdraws a candidate even when the observed condition is in its reason codes', () => {
    const candidate = target({ revision: sha, reason_codes: ['pr_draft'] });
    expect(isApprovalCandidate(reconcileApprovalTarget(candidate, pr({ draft: true })), 100)).toBe(false);
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

it('preserves stale detail against an older current projection in either reconciliation order', () => {
  const current = target({ target_id: 't', revision: sha, validated_at: new Date(1000).toISOString(), reason_codes: [] });
  const stale = { ...current, freshness_state: 'stale', reason_codes: ['base_changed'] };
  for (const [detail, projection] of [[stale, current], [current, stale]]) {
    const result = reconcileApprovalTarget(detail, pr(), projection);
    expect(result.freshness_state).toBe('stale');
    expect(result.reason_codes).toContain('base_changed');
  }
});
it('accepts a newer successful renewal without reviving stale evidence', () => {
  const expired = target({ target_id: 't', revision: sha, freshness_state: 'expired', validated_at: new Date(1000).toISOString(), valid_until: new Date(2000).toISOString() });
  const renewed = { ...expired, freshness_state: 'current', validated_at: new Date(3000).toISOString(), valid_until: new Date(4000).toISOString() };
  expect(reconcileApprovalTarget(expired, pr(), renewed).freshness_state).toBe('current');
  expect(reconcileApprovalTarget(renewed, pr(), expired).valid_until).toBe(renewed.valid_until);
  expect(reconcileApprovalTarget({ ...expired, freshness_state: 'stale' }, pr(), renewed).freshness_state).toBe('stale');
});
