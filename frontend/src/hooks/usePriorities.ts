import { useQuery } from '@tanstack/react-query';
import { fetchPriorities } from '@/api/priorities';
import { PRIORITY_POLL_INTERVAL, PRIORITY_STALE_TIME } from '@/utils/constants';

export function usePriorities() {
  return useQuery({
    queryKey: ['priorities'],
    queryFn: fetchPriorities,
    refetchInterval: PRIORITY_POLL_INTERVAL,
    staleTime: PRIORITY_STALE_TIME,
  });
}
