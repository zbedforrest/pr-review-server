import { useMutation, useQueryClient } from '@tanstack/react-query';
import { QUICK_ACTION_STATE, submitQuickAction, type QuickActionParams } from '@/api/prActions';
import type { PR } from '@/types/pr';

function applyOptimisticReview(pr: PR, action: QuickActionParams['action']): PR {
  const state = QUICK_ACTION_STATE[action];
  const bump = action === 'approve' && pr.my_review_status !== 'APPROVED' ? 1 : 0;
  return { ...pr, my_review_status: state, approval_count: pr.approval_count + bump };
}

/**
 * Posts a review to GitHub as the signed-in user. The row flips optimistically
 * and rolls back on any error; the caller presents errors.
 */
export function useSubmitQuickAction() {
  const queryClient = useQueryClient();

  return useMutation({
    mutationFn: (params: QuickActionParams) => submitQuickAction(params),
    onMutate: async (params) => {
      await queryClient.cancelQueries({ queryKey: ['prs'] });
      const previousPRs = queryClient.getQueryData<PR[]>(['prs']);
      queryClient.setQueryData<PR[]>(['prs'], (old) =>
        old?.map((pr) =>
          pr.owner === params.owner && pr.repo === params.repo && pr.number === params.number
            ? applyOptimisticReview(pr, params.action)
            : pr
        )
      );
      return { previousPRs };
    },
    onError: (_err, _variables, context) => {
      if (context?.previousPRs) {
        queryClient.setQueryData(['prs'], context.previousPRs);
      }
    },
    onSettled: () => {
      queryClient.invalidateQueries({ queryKey: ['prs'] });
    },
  });
}
