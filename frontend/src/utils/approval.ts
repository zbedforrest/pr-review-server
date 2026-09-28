import type { PR } from '@/types/pr';
import type { ApprovalTarget } from '@/types/approval';
import { filterAndSortPRs, type PRFilterCriteria } from './sectionFilters';

export const approvalKey = (pr: { owner: string; repo: string; number: number }) => `${pr.owner}/${pr.repo}/${pr.number}`.toLowerCase();
export function approvalScope(prs: PR[], filters: PRFilterCriteria, username?: string): PR[] {
  const seen = new Set<string>();
  return filterAndSortPRs(prs, filters, username).filter(pr => {
    const key = approvalKey(pr);
    if (seen.has(key)) return false;
    seen.add(key);
    return !pr.is_mine && !pr.hidden && !pr.draft && (!pr.pr_state || pr.pr_state === 'open') &&
      !(pr.my_review_status === 'APPROVED' && pr.my_review_commit_sha === pr.commit_sha) && /^[0-9a-f]{40}$/i.test(pr.commit_sha);
  });
}
export function isApprovalCandidate(target: ApprovalTarget, now = Date.now()): boolean {
  return target.execution_status === 'completed' && target.decision === 'candidate' &&
    target.freshness_state === 'current' && !!target.valid_until && Date.parse(target.valid_until) > now;
}
export function approvalBucket(target: ApprovalTarget, now = Date.now()): string {
  if (['failed', 'timed_out', 'cancelled'].includes(target.execution_status)) return target.execution_status;
  if (target.execution_status !== 'completed') return target.execution_status;
  if (target.freshness_state === 'stale' || (target.decision === 'candidate' && !isApprovalCandidate(target, now))) return 'stale';
  return target.decision || 'insufficient_evidence';
}
export function safeEvidenceURL(value: string): string | undefined {
  try { const url = new URL(value, window.location.origin); if (url.protocol !== 'https:' && url.origin !== window.location.origin) return undefined;
    if (url.origin === window.location.origin && url.pathname.startsWith('/api/v1/approval-')) return url.href;
    if (url.protocol === 'https:' && url.hostname === 'github.com' && !url.username && !url.password) return url.href;
  } catch { return undefined; }
  return undefined;
}

export function reconcileApprovalTarget(target: ApprovalTarget, pr: PR, latest?: ApprovalTarget): ApprovalTarget {
  const current = latest?.target_id === target.target_id
    ? { ...target, ...latest, assessment: target.assessment, snapshot: target.snapshot }
    : { ...target };
  let reason = '';
  if (latest && latest.target_id !== target.target_id) reason = 'superseded';
  else if (pr.commit_sha !== target.revision) reason = 'head_changed';
  else if (pr.hidden) reason = 'hidden';
  else if (pr.draft) reason = 'draft';
  else if (pr.pr_state && pr.pr_state !== 'open') reason = 'closed';
  else if (pr.is_mine) reason = 'self_authored';
  else if (pr.ci_state === 'failure') reason = 'ci_failed';
  else if (pr.ci_state === 'pending') reason = 'ci_pending';
  else if (pr.review_decision === 'CHANGES_REQUESTED' || pr.my_review_status === 'CHANGES_REQUESTED') reason = 'human_changes_requested';
  else if (['pending', 'generating', 'agent_reviewing'].includes(pr.status)) reason = 'review_in_progress';
  else if (pr.my_review_status === 'APPROVED' && pr.my_review_commit_sha === pr.commit_sha) reason = 'already_approved';
  if (reason) return { ...current, freshness_state: 'stale', reason_codes: Array.from(new Set([...(current.reason_codes || []), reason])) };
  return current;
}
