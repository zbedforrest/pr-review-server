import { usePRs } from '@/hooks/usePRs';
import { PRTable } from './PRTable';
import { LoadingSpinner, ErrorMessage } from '@/components/common';

export function ReviewPRsSection() {
  const { data: prs, isLoading, error } = usePRs();

  const reviewPRs = prs?.filter((pr) => !pr.is_mine) || [];

  return (
    <section>
      <h2>PRs to Review ({reviewPRs.length})</h2>
      {isLoading && <LoadingSpinner />}
      {error && <ErrorMessage message={`Error loading PRs: ${error.message}`} />}
      {!isLoading && !error && <PRTable prs={reviewPRs} showMyReview={true} />}
    </section>
  );
}
