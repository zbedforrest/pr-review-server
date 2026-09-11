import { useNeedsReReview } from '@/hooks/useNeedsReReview';
import { PRTable } from './PRTable';
import './SectionHeader.scss';
import './NeedsReReviewSection.scss';

export function NeedsReReviewSection() {
  const { rows, count } = useNeedsReReview();

  if (count === 0) return null;

  return (
    <section className="needs-re-review">
      <div className="section-header">
        <h2>Needs your re-review ({count})</h2>
      </div>
      <p className="needs-re-review__explanation">
        PRs where the author pushed after you requested changes.
      </p>
      <PRTable prs={rows} variant="attention" />
    </section>
  );
}
