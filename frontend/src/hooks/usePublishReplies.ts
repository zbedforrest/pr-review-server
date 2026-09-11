import { useQuery } from '@tanstack/react-query';
import { fetchPublishReplies } from '@/api/publishReplies';

export function usePublishReplies(enabled: boolean) {
  return useQuery({
    queryKey: ['publish-replies'],
    queryFn: fetchPublishReplies,
    enabled,
    staleTime: 30 * 1000,
  });
}
