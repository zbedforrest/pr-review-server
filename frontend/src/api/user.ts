import { apiGet } from './client';

export interface CurrentUser {
  id: number;
  github_username: string;
  github_avatar_url: string;
  is_admin: boolean;
}

export async function fetchCurrentUser(): Promise<CurrentUser> {
  return apiGet<CurrentUser>('/api/user');
}
