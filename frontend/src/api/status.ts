import { apiGet } from './client';
import type { ServerStatus } from '@/types/status';

export async function fetchStatus(): Promise<ServerStatus> {
  return apiGet<ServerStatus>('/api/status');
}
