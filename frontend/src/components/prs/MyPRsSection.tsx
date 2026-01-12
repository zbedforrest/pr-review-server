import { usePRs } from '@/hooks/usePRs';
import { PRTable } from './PRTable';
import { LoadingSpinner, ErrorMessage } from '@/components/common';

export function MyPRsSection() {
  const { data: prs, isLoading, error } = usePRs();

  const myPRs = prs?.filter((pr) => pr.is_mine) || [];

  return (
    <section>
      <h2>My PRs ({myPRs.length})</h2>
      {isLoading && <LoadingSpinner />}
      {error && <ErrorMessage message={`Error loading PRs: ${error.message}`} />}
      {!isLoading && !error && <PRTable prs={myPRs} showMyReview={false} />}
    </section>
  );
}
