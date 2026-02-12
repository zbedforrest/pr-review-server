import { useQuery } from '@tanstack/react-query';
import { fetchTelemetryStats } from '@/api/telemetry';

export function useTelemetryStats(days: number) {
  return useQuery({
    queryKey: ['telemetry-stats', days],
    queryFn: () => fetchTelemetryStats(days),
    staleTime: 30 * 1000, // 30 seconds
  });
}
