import type { QuickAction } from '@/api/prActions';
import type { CurrentUser } from '@/api/user';
import type { PR } from '@/types/pr';

export type QuickActionKey = QuickAction | 'open' | 'copy';

export interface QuickActionAvailability {
  enabled: boolean;
  /** Shown as the disabled item's tooltip. */
  reason?: string;
}

export type QuickActionAvailabilityMap = Record<QuickActionKey, QuickActionAvailability>;

export type QuickActionUser = Pick<CurrentUser, 'quick_actions_enabled' | 'github_actions_available'>;

export const SIGN_IN_REASON = 'Sign in again to enable GitHub actions';
export const FLAG_OFF_REASON = 'Quick actions are not enabled';
export const CLIPBOARD_UNAVAILABLE_REASON = 'Clipboard unavailable';

const ok: QuickActionAvailability = { enabled: true };
const off = (reason: string): QuickActionAvailability => ({ enabled: false, reason });

export function isGreenForDraftApprove(pr: PR): { prism: boolean; greptile: boolean } {
  const prism =
    (pr.review_verdict === 'approve' || pr.review_verdict === 'approve_suggestions') && pr.critical_count === 0;
  return { prism, greptile: pr.greptile_status === 'green' };
}

function draftApproveReason(pr: PR): string | null {
  const { prism, greptile } = isGreenForDraftApprove(pr);
  if (prism && greptile) return null;
  const notGreen = !prism && !greptile ? 'PRism and Greptile are' : !prism ? 'PRism is' : 'Greptile is';
  return `Draft PR: approve needs PRism and Greptile green on this head (${notGreen} not green)`;
}

/**
 * Which leaf items are enabled for this row and user, with the tooltip for
 * each disabled one. Pure: the server re-checks the same rules.
 */
export function quickActionAvailability(
  pr: PR,
  user: QuickActionUser | undefined,
  clipboardAvailable = typeof navigator !== 'undefined' && !!navigator.clipboard
): QuickActionAvailabilityMap {
  const always = { open: ok, copy: clipboardAvailable ? ok : off(CLIPBOARD_UNAVAILABLE_REASON) };

  if (!user?.quick_actions_enabled) {
    const reason = off(FLAG_OFF_REASON);
    return { approve: reason, request_changes: reason, comment: reason, ...always };
  }

  const state = pr.pr_state ?? 'open';
  if (state !== 'open') {
    const reason = off(`PR is ${state}`);
    return { approve: reason, request_changes: reason, comment: reason, ...always };
  }

  if (user.github_actions_available !== true) {
    const reason = off(SIGN_IN_REASON);
    return { approve: reason, request_changes: reason, comment: reason, ...always };
  }

  if (pr.is_mine) {
    return {
      approve: off('You cannot approve your own PR'),
      request_changes: off('You cannot request changes on your own PR'),
      comment: ok,
      ...always,
    };
  }

  const draftReason = pr.draft ? draftApproveReason(pr) : null;
  return { approve: draftReason ? off(draftReason) : ok, request_changes: ok, comment: ok, ...always };
}
