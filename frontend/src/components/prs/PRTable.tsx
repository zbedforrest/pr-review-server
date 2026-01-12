import type { PR } from '@/types/pr';
import { PRTableRow } from './PRTableRow';

interface PRTableProps {
  prs: PR[];
  showMyReview?: boolean;
}

export function PRTable({ prs, showMyReview = false }: PRTableProps) {
  if (prs.length === 0) {
    return <p>No PRs found.</p>;
  }

  return (
    <div className="pr-table-container">
      <table className="pr-table">
        <thead>
          <tr>
            <th>PR</th>
            <th>Author</th>
            <th>Commit</th>
            <th>Status</th>
            {showMyReview && <th>My Review</th>}
            <th>Notes</th>
            <th>Approvals</th>
            <th>Review</th>
            <th>Actions</th>
          </tr>
        </thead>
        <tbody>
          {prs.map((pr) => (
            <PRTableRow
              key={`${pr.owner}/${pr.repo}/${pr.number}`}
              pr={pr}
              showMyReview={showMyReview}
            />
          ))}
        </tbody>
      </table>
    </div>
  );
}
