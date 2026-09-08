import { apiGet, apiPost } from './client';

export interface Settings {
  auto_review_requested_prs: boolean;
  // GitHub logins whose PRs may receive posted reviews, comma-separated, or
  // "*" for everyone. Optional: older servers omit the publish fields.
  publish_enabled_authors?: string;
  publish_inline_cap?: number;
  publish_inline_min_severity?: string;
}

export async function fetchSettings(): Promise<Settings> {
  return apiGet<Settings>('/api/settings');
}

export async function updateSettings(settings: Partial<Settings>): Promise<Settings> {
  return apiPost<Settings>('/api/settings', settings);
}
