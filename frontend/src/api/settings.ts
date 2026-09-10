import { apiGet, apiPost } from './client';

export type ReplyMode = 'off' | 'observe' | 'react' | 'shadow' | 'respond';

export interface Settings {
  auto_review_requested_prs: boolean;
  review_n_requests: number;
  // GitHub logins whose PRs may receive posted reviews, comma-separated, or
  // "*" for everyone.
  publish_enabled_authors: string;
  publish_inline_cap: number;
  publish_inline_min_severity: string;
  publish_show_unverified: boolean;
  publish_reply_mode: ReplyMode;
  // RFC3339 stamp of when replies were last switched on; "" whenever the mode is off.
  publish_reply_enabled_at: string;
  admin_logins: string;
  admin_logins_fixed: string[];
  // Legacy flag the server still returns; never shown in the UI.
  generate_html?: boolean;
}

export async function fetchSettings(): Promise<Settings> {
  return apiGet<Settings>('/api/settings');
}

export async function updateSettings(settings: Partial<Settings>): Promise<Settings> {
  return apiPost<Settings>('/api/settings', settings);
}
