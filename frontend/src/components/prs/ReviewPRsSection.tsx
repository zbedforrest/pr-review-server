import { usePRs } from '@/hooks/usePRs';
import { PRTable } from './PRTable';
import { LoadingSpinner, ErrorMessage } from '@/components/common';

export function ReviewPRsSection() {
  const { data: prs, isLoading, error } = usePRs();

  // Filter and sort: newest PRs first (oldest at bottom)
  const reviewPRs = (prs?.filter((pr) => !pr.is_mine) || []).sort((a, b) => {
    // PRs without created_at go to the end
    if (!a.created_at && !b.created_at) return 0;
    if (!a.created_at) return 1;
    if (!b.created_at) return -1;

    // Sort by created_at descending (newest first)
    return new Date(b.created_at).getTime() - new Date(a.created_at).getTime();
  });

  return (
    <section>
      <h2>PRs to Review ({reviewPRs.length})</h2>
      {isLoading && <LoadingSpinner />}
      {error && <ErrorMessage message={`Error loading PRs: ${error.message}`} />}
      {!isLoading && !error && <PRTable prs={reviewPRs} showMyReview={true} />}
    </section>
  );
}
