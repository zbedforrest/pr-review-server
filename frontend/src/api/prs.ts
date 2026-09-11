import { apiGet, apiPost } from './client';
import type { PR } from '@/types/pr';

export async function fetchPRs(): Promise<PR[]> {
  return apiGet<PR[]>('/api/prs');
}

export interface DeletePRParams {
  owner: string;
  repo: string;
  number: number;
}

export async function deletePR(params: DeletePRParams): Promise<{ status: string }> {
  return apiPost<{ status: string }>('/api/prs/delete', params);
}

export interface UpdatePRNotesParams {
  owner: string;
  repo: string;
  number: number;
  notes: string;
}

export async function updatePRNotes(params: UpdatePRNotesParams): Promise<{ status: string; notes: string }> {
  return apiPost<{ status: string; notes: string }>('/api/prs/notes', params);
}

export interface SetPRHiddenParams {
  owner: string;
  repo: string;
  number: number;
  hidden: boolean;
}

export async function setPRHidden(params: SetPRHiddenParams): Promise<{ status: string; hidden: boolean }> {
  return apiPost<{ status: string; hidden: boolean }>('/api/prs/hidden', params);
}

export interface TriggerReviewParams {
  owner: string;
  repo: string;
  number: number;
  /** Post the review to the GitHub PR. Omitted or true = post; false = dashboard only. */
  publish?: boolean;
}

// The server treats a missing key as "publish"; sending publish: undefined
// would serialize to nothing anyway, but dropping it keeps the body explicit.
function reviewRequestBody({ publish, ...rest }: TriggerReviewParams): Record<string, unknown> {
  return publish === undefined ? rest : { ...rest, publish };
}

export async function triggerReview(params: TriggerReviewParams): Promise<{ status: string }> {
  return apiPost<{ status: string }>('/api/prs/trigger-review', reviewRequestBody(params));
}

export interface GenerateReviewResponse {
  status: string;
  commit: string;
  state: string;
  merged: boolean;
  review_url: string;
  findings_url: string;
}

// Unlike triggerReview, this works for PRs the poller hasn't ingested
// (merged/closed PRs, or any repo the server can read): the server fetches
// the PR from GitHub, upserts it, and starts the review in the background.
//
// source: "form" marks a deliberate paste into the dashboard's URL input —
// the only origin that claims the PR into the Requested by Me section.
// API/skill callers omit it and stay off the requester's dashboard.
export async function generateReview(params: TriggerReviewParams): Promise<GenerateReviewResponse> {
  return apiPost<GenerateReviewResponse>('/api/prs/generate-review', { ...reviewRequestBody(params), source: 'form' });
}
