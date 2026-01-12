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
