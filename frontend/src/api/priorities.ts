import { apiGet } from './client';
import type { PriorityResult } from '@/types/priority';

export async function fetchPriorities(): Promise<PriorityResult> {
  return apiGet<PriorityResult>('/api/priorities');
}
