import { apiPost } from './client';

export type QuickAction = 'approve' | 'request_changes' | 'comment';

export type QuickActionState = 'APPROVED' | 'CHANGES_REQUESTED' | 'COMMENTED';

export type QuickActionErrorCode =
  | 'reauth_required'
  | 'own_pr'
  | 'pr_closed'
  | 'pr_unknown'
  | 'head_moved'
  | 'duplicate'
  | 'no_permission'
  | 'rate_limited'
  | 'validation'
  | 'draft_not_green'
  | 'github_error';

export interface QuickActionParams {
  owner: string;
  repo: string;
  number: number;
  action: QuickAction;
  /** Required for request_changes and comment; omitted when empty. */
  body?: string;
  /** pr.commit_sha at click time; the server answers 409 head_moved if it differs. */
  expected_head_sha: string;
  /** One per submit attempt so a retry after head_moved is not replayed. */
  request_id: string;
}

export interface QuickActionResponse {
  status: 'success';
  action: QuickAction;
  review_id: number;
  html_url: string;
  state: QuickActionState;
  head_sha: string;
  actor: string;
}

export const QUICK_ACTION_STATE: Record<QuickAction, QuickActionState> = {
  approve: 'APPROVED',
  request_changes: 'CHANGES_REQUESTED',
  comment: 'COMMENTED',
};

export function newRequestId(): string {
  if (typeof crypto !== 'undefined' && typeof crypto.randomUUID === 'function') {
    return crypto.randomUUID();
  }
  return `${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 12)}`;
}

export async function submitQuickAction(params: QuickActionParams): Promise<QuickActionResponse> {
  const { body, ...rest } = params;
  const trimmed = body?.trim();
  return apiPost<QuickActionResponse>('/api/prs/quick-action', trimmed ? { ...rest, body: trimmed } : rest);
}
