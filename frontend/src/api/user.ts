import { apiDelete, apiGet, apiPost } from './client';

export interface CurrentUser {
  id: number;
  github_username: string;
  github_avatar_url: string;
  is_admin: boolean;
  // Quick actions feature flag and whether this session holds a GitHub token
  // that can act as the user. Optional so older servers still type-check.
  quick_actions_enabled?: boolean;
  github_actions_available?: boolean;
  // Standing with the publish gate: enrolled means publish_enabled_authors
  // admits the login (via "login", a team slug, or "*"); opted out means the
  // login asked PRism to stop posting. Dashboard reviews continue either way.
  publish_enrolled?: boolean;
  publish_opted_out?: boolean;
  enrolled_via?: string;
}

export const PUBLISH_OPT_OUT_PATH = '/api/me/publish-opt-out';

export async function fetchCurrentUser(): Promise<CurrentUser> {
  return apiGet<CurrentUser>('/api/user');
}

export async function leavePublishing(): Promise<CurrentUser> {
  return apiPost<CurrentUser>(PUBLISH_OPT_OUT_PATH, {});
}

export async function rejoinPublishing(): Promise<CurrentUser> {
  return apiDelete<CurrentUser>(PUBLISH_OPT_OUT_PATH, {});
}
