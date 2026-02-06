import type { PR } from '@/types/pr';
import { PRTableRow } from './PRTableRow';

interface PRTableProps {
  prs: PR[];
  showReviewColumns?: boolean;
  showViaTeams?: boolean;
}

export function PRTable({ prs, showReviewColumns = true, showViaTeams = true }: PRTableProps) {
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
            {showReviewColumns && <th>Status</th>}
            <th>CI</th>
            <th>Approvals</th>
            <th>My Review</th>
            {showViaTeams && <th>Via Teams</th>}
            <th>Notes</th>
            {showReviewColumns && <th>Review</th>}
            <th>Actions</th>
          </tr>
        </thead>
        <tbody>
          {prs.map((pr) => (
            <PRTableRow
              key={`${pr.owner}/${pr.repo}/${pr.number}`}
              pr={pr}
              showReviewColumns={showReviewColumns}
              showViaTeams={showViaTeams}
            />
          ))}
        </tbody>
      </table>
    </div>
  );
}
