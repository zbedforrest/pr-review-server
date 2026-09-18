import { APIError } from '@/api/client';
import type { QuickAction } from '@/api/prActions';

export const QUICK_ACTION_BODY_MAX = 20000;
export const QUICK_ACTION_COUNTER_THRESHOLD = 18000;

interface ActionCopy {
  /** Sentence lead for the dialog title, e.g. "Approve acme/example #1". */
  title: string;
  verb: string;
  pending: string;
  requiresBody: boolean;
  tone: 'success' | 'danger' | 'neutral';
}

export const QUICK_ACTION_COPY: Record<QuickAction, ActionCopy> = {
  approve: { title: 'Approve', verb: 'Approve', pending: 'Approving…', requiresBody: false, tone: 'success' },
  request_changes: {
    title: 'Request changes on',
    verb: 'Request changes',
    pending: 'Requesting changes…',
    requiresBody: true,
    tone: 'danger',
  },
  comment: { title: 'Comment on', verb: 'Comment', pending: 'Commenting…', requiresBody: true, tone: 'neutral' },
};

export const shortSha = (sha: string) => sha.slice(0, 7);

export function quickActionErrorMessage(action: QuickAction, error: APIError | Error): string {
  const code = error instanceof APIError ? error.code : undefined;
  switch (code) {
    case 'own_pr':
      return action === 'request_changes'
        ? 'GitHub does not allow requesting changes on your own PR.'
        : 'GitHub does not allow approving your own PR.';
    case 'no_permission':
      return 'Your GitHub account cannot review this repository.';
    case 'reauth_required':
      return 'Your GitHub authorization expired. Sign in again.';
    case 'pr_closed':
      return 'This PR is closed on GitHub.';
    case 'head_moved':
      return 'The PR head moved since this row loaded. Close this dialog and try again from the refreshed row.';
    case 'pr_unknown':
      return 'PRism no longer tracks this PR.';
    case 'draft_not_green':
      return 'Draft PR: approve needs PRism and Greptile green on this head.';
    case 'duplicate':
      return 'This action was just submitted; check the PR on GitHub before retrying.';
    case 'rate_limited': {
      const seconds = Number((error as APIError).details?.retry_after_seconds);
      const minutes = Number.isFinite(seconds) && seconds > 0 ? Math.max(1, Math.ceil(seconds / 60)) : null;
      return minutes ? `GitHub rate limit reached, try again in ${minutes} minute${minutes === 1 ? '' : 's'}.` : 'GitHub rate limit reached, try again in a few minutes.';
    }
    case 'validation':
      return error.message;
    default:
      return `GitHub returned an error: ${error.message}`;
  }
}
