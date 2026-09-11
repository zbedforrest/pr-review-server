import { apiGet, apiPost } from './client';

// Outcome triage (forensics / label harvest): record what a human decided
// about a review finding. The backend endpoint is feature-gated
// (FINDING_OUTCOMES_ENABLED); when disabled it returns 404, which callers
// treat as "feature hidden".

export type FindingOutcomeKind = 'dismissed' | 'acknowledged' | 'unresolved';

export interface FindingOutcome {
  owner: string;
  repo: string;
  number: number;
  reviewed_sha: string;
  // "<file>:<line/10>:<hash>" — computed server-side from the raw finding.
  fingerprint: string;
  provenance?: string;
  severity?: string;
  outcome: FindingOutcomeKind;
  reason?: string;
  decided_by: string;
  decided_at: string;
}

export async function fetchFindingOutcomes(
  owner: string,
  repo: string,
  number: number
): Promise<FindingOutcome[]> {
  const params = new URLSearchParams({ owner, repo, number: String(number) });
  const resp = await apiGet<{ outcomes: FindingOutcome[] }>(
    `/api/prs/finding-outcomes?${params.toString()}`
  );
  return resp.outcomes ?? [];
}

export interface RecordFindingOutcomeParams {
  owner: string;
  repo: string;
  number: number;
  /** The commit the review that surfaced the finding was generated at. */
  sha: string;
  file: string;
  line: number;
  comment: string;
  severity: string;
  provenance?: string;
  outcome: FindingOutcomeKind;
  reason?: string;
}

export async function recordFindingOutcome(
  params: RecordFindingOutcomeParams
): Promise<{ status: string; outcome: FindingOutcome }> {
  return apiPost<{ status: string; outcome: FindingOutcome }>(
    '/api/prs/finding-outcomes',
    params
  );
}

// One structured finding from the review's JSON sidecar, as served by
// GET /api/review/{owner}/{repo}/{number} (context fields omitted — the
// triage list only needs identity + comment).
export interface ReviewFinding {
  severity: string;
  provenance?: string;
  file: string;
  line: number;
  comment: string;
  /** Schema 2: what the review concluded and whether it asserts the finding as a claim. */
  state?: string;
  active?: boolean;
}

interface ReviewFindingsResponse {
  commit_sha?: string;
  findings_available: boolean;
  findings?: ReviewFinding[];
}

/**
 * Fetch the review's CRITICAL findings (triage is CRITICAL-only at launch).
 * Returns the reviewed commit SHA alongside so outcomes are recorded
 * against the review that surfaced the finding, not the PR's moving HEAD.
 */
export async function fetchCriticalFindings(
  owner: string,
  repo: string,
  number: number
): Promise<{ sha: string; findings: ReviewFinding[] }> {
  const resp = await apiGet<ReviewFindingsResponse>(`/api/review/${owner}/${repo}/${number}`);
  const findings = (resp.findings ?? []).filter(f => f.severity === 'critical' && f.active !== false);
  return { sha: resp.commit_sha ?? '', findings };
}
