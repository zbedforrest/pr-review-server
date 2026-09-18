import { useMutation, useQueryClient } from '@tanstack/react-query';
import { QUICK_ACTION_STATE, submitQuickAction, type QuickActionParams } from '@/api/prActions';
import type { PR } from '@/types/pr';

type ReviewFields = Pick<PR, 'my_review_status' | 'approval_count'>;

// approval_count counts users whose latest review is APPROVED, so leaving
// that state drops the count just as entering it raises it.
function applyOptimisticReview(pr: PR, action: QuickActionParams['action']): PR {
  const state = QUICK_ACTION_STATE[action];
  const delta = Number(state === 'APPROVED') - Number(pr.my_review_status === 'APPROVED');
  return { ...pr, my_review_status: state, approval_count: pr.approval_count + delta };
}

const isTarget = (pr: PR, params: QuickActionParams) =>
  pr.owner === params.owner && pr.repo === params.repo && pr.number === params.number;

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
      const target = queryClient.getQueryData<PR[]>(['prs'])?.find((pr) => isTarget(pr, params));
      const previous: ReviewFields | undefined = target && {
        my_review_status: target.my_review_status,
        approval_count: target.approval_count,
      };
      queryClient.setQueryData<PR[]>(['prs'], (old) =>
        old?.map((pr) => (isTarget(pr, params) ? applyOptimisticReview(pr, params.action) : pr))
      );
      return { previous };
    },
    // Restore only the target row's review fields so updates to other rows
    // that landed while GitHub was answering survive the rollback.
    onError: (_err, params, context) => {
      const previous = context?.previous;
      if (!previous) return;
      queryClient.setQueryData<PR[]>(['prs'], (old) =>
        old?.map((pr) => (isTarget(pr, params) ? { ...pr, ...previous } : pr))
      );
    },
    onSettled: () => {
      queryClient.invalidateQueries({ queryKey: ['prs'] });
    },
  });
}
