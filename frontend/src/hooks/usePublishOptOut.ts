import { useMutation, useQueryClient } from '@tanstack/react-query';
import { leavePublishing, rejoinPublishing, type CurrentUser } from '@/api/user';

// Leave and Rejoin answer with the refreshed user document, which replaces
// the cached one so the banner flips without a second request.
export function usePublishOptOut() {
  const queryClient = useQueryClient();
  const settle = (user: CurrentUser) => queryClient.setQueryData<CurrentUser>(['currentUser'], user);
  const leave = useMutation({ mutationFn: leavePublishing, onSuccess: settle });
  const rejoin = useMutation({ mutationFn: rejoinPublishing, onSuccess: settle });
  return { leave, rejoin };
}
