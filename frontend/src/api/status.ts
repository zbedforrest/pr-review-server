import { apiGet } from './client';
import type { ServerStatus } from '@/types/status';

export async function fetchStatus(): Promise<ServerStatus> {
  const status = await apiGet<ServerStatus>('/api/status');
  return {
    ...status,
    snapshot_received_at_ms: Date.now(),
  };
}
