import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query';
import { fetchPRs, deletePR, type DeletePRParams } from '@/api/prs';
import type { PR } from '@/types/pr';
import { PR_POLL_INTERVAL, PR_STALE_TIME } from '@/utils/constants';

export function usePRs() {
  return useQuery({
    queryKey: ['prs'],
    queryFn: fetchPRs,
    refetchInterval: PR_POLL_INTERVAL,
    staleTime: PR_STALE_TIME,
  });
}

export function useDeletePR() {
  const queryClient = useQueryClient();

  return useMutation({
    mutationFn: (params: DeletePRParams) => deletePR(params),
    onMutate: async (params) => {
      // Cancel outgoing refetches
      await queryClient.cancelQueries({ queryKey: ['prs'] });

      // Snapshot previous value
      const previousPRs = queryClient.getQueryData<PR[]>(['prs']);

      // Optimistically update
      queryClient.setQueryData<PR[]>(['prs'], (old) =>
        old?.filter(
          (pr) =>
            !(
              pr.owner === params.owner &&
              pr.repo === params.repo &&
              pr.number === params.number
            )
        )
      );

      return { previousPRs };
    },
    onError: (err, _variables, context) => {
      // Rollback on error
      if (context?.previousPRs) {
        queryClient.setQueryData(['prs'], context.previousPRs);
      }
      alert(`Error deleting PR: ${err.message}`);
    },
    onSettled: () => {
      queryClient.invalidateQueries({ queryKey: ['prs'] });
    },
  });
}
