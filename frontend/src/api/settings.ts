import { apiGet, apiPost } from './client';
import type { ReviewProfile } from '@/types/pr';

export type ReplyMode = 'off' | 'observe' | 'react' | 'shadow' | 'respond';

export const AUTO_REVIEW_TRIGGERS = ['ready_for_review', 'opened', 'synchronize', 'poll_fallback'] as const;
export type AutoReviewTrigger = (typeof AUTO_REVIEW_TRIGGERS)[number];

// Per-trigger profile overrides; "" means the deployment default applies.
export type AutoReviewProfileByTrigger = Record<AutoReviewTrigger, ReviewProfile | ''> & {
  repos: Record<string, Partial<Record<AutoReviewTrigger, ReviewProfile>>>;
};

export const EMPTY_PROFILE_BY_TRIGGER: AutoReviewProfileByTrigger = {
  ready_for_review: '', opened: '', synchronize: '', poll_fallback: '', repos: {},
};

export interface Settings {
  auto_review_requested_prs: boolean;
  review_n_requests: number;
  // GitHub logins whose PRs may receive posted reviews, comma-separated, or
  // "*" for everyone.
  publish_enabled_authors: string;
  publish_inline_cap: number;
  publish_inline_min_severity: string;
  publish_show_unverified: boolean;
  // Review and comment automatically when an allowlisted author's PR becomes
  // ready for review, opens ready, or gets a new push.
  auto_review_ready_prs: boolean;
  publish_reply_mode: ReplyMode;
  // RFC3339 stamp of when replies were last switched on; "" whenever the mode is off.
  publish_reply_enabled_at: string;
  admin_logins: string;
  admin_logins_fixed: string[];
  // Review profile per automatic trigger, and the authors whose automatic
  // reviews may run a lite profile. Optional: older servers omit them.
  auto_review_profile_by_trigger?: AutoReviewProfileByTrigger;
  auto_review_lite_authors?: string;
  // Read-only: what "default" resolves to (REVIEW_DEFAULT_PROFILE).
  review_default_profile?: ReviewProfile;
  review_profiles?: ReviewProfile[];
  // Legacy flag the server still returns; never shown in the UI.
  generate_html?: boolean;
}

export async function fetchSettings(): Promise<Settings> {
  return apiGet<Settings>('/api/settings');
}

export async function updateSettings(settings: Partial<Settings>): Promise<Settings> {
  return apiPost<Settings>('/api/settings', settings);
}
