import { apiGet } from './client';

export interface CurrentUser {
  id: number;
  github_username: string;
  github_avatar_url: string;
  is_admin: boolean;
  // Quick actions feature flag and whether this session holds a GitHub token
  // that can act as the user. Optional so older servers still type-check.
  quick_actions_enabled?: boolean;
  github_actions_available?: boolean;
}

export async function fetchCurrentUser(): Promise<CurrentUser> {
  return apiGet<CurrentUser>('/api/user');
}
