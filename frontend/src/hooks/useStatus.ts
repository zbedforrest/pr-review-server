import { useQuery } from '@tanstack/react-query';
import { fetchStatus } from '@/api/status';
import { STATUS_POLL_INTERVAL, STATUS_STALE_TIME } from '@/utils/constants';

export function useStatus() {
  return useQuery({
    queryKey: ['status'],
    queryFn: fetchStatus,
    refetchInterval: STATUS_POLL_INTERVAL,
    staleTime: STATUS_STALE_TIME,
  });
}
