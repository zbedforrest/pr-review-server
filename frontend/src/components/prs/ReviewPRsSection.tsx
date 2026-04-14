import { usePRs } from '@/hooks/usePRs';
import { useSettings, useUpdateSettings } from '@/hooks/useSettings';
import { useTelemetry } from '@/hooks/useTelemetry';
import { PRTable } from './PRTable';
import { LoadingSpinner, ErrorMessage } from '@/components/common';
import { PR } from '@/types/pr';
import { TriageFilter, categorizePR } from './triageUtils';
import './SectionHeader.scss';

interface ReviewPRsSectionProps {
  showReviewColumns?: boolean;
  onToggleColumns?: () => void;
  searchTerm?: string;
  triageFilter?: TriageFilter | null;
}

// Helper to filter and sort PRs
function filterAndSortPRs(prs: PR[], searchTerm: string): PR[] {
  return prs
    .filter((pr) => {
      if (!searchTerm) return true;
      const lowerTerm = searchTerm.toLowerCase();
      return (
        pr.title.toLowerCase().includes(lowerTerm) ||
        pr.repo.toLowerCase().includes(lowerTerm) ||
        pr.owner.toLowerCase().includes(lowerTerm) ||
        pr.author.toLowerCase().includes(lowerTerm) ||
        pr.number.toString().includes(lowerTerm)
      );
    })
    .sort((a, b) => {
      // PRs without created_at go to the end
      if (!a.created_at && !b.created_at) return 0;
      if (!a.created_at) return 1;
      if (!b.created_at) return -1;
      // Sort by created_at descending (newest first)
      return new Date(b.created_at).getTime() - new Date(a.created_at).getTime();
    });
}

export function ReviewPRsSection({ showReviewColumns = true, onToggleColumns, searchTerm = '', triageFilter = null }: ReviewPRsSectionProps) {
  const { data: prs, isLoading, error } = usePRs();
  const { data: settings } = useSettings();
  const updateSettings = useUpdateSettings();
  const { track } = useTelemetry();

  // Split PRs into "My PRs" and "PRs to Review"
  const allPRs = prs || [];
  const myPRs = filterAndSortPRs(allPRs.filter(pr => pr.is_mine), searchTerm);

  // Apply triage filter to review PRs
  let reviewPRs = filterAndSortPRs(allPRs.filter(pr => !pr.is_mine), searchTerm);
  if (triageFilter) {
    reviewPRs = reviewPRs.filter(pr => {
      // Only filter completed PRs by triage category
      if (pr.status !== 'completed') return true;
      return categorizePR(pr) === triageFilter;
    });
  }

  const handleToggleAutoReview = () => {
    if (!settings) return;
    const next = !settings.auto_review_requested_prs;
    track('toggle_auto_review', { label: next ? 'on' : 'off' });
    updateSettings.mutate({
      auto_review_requested_prs: next
    });
  };

  const autoReviewEnabled = settings?.auto_review_requested_prs ?? true;

  const showSyncBanner = !isLoading && !error && allPRs.length === 0;

  return (
    <>
      {showSyncBanner && (
        <div style={{
          background: '#3d3520',
          border: '1px solid #6e5c2e',
          borderRadius: '6px',
          padding: '16px',
          marginBottom: '24px',
          color: '#e3b341',
          textAlign: 'center',
        }}>
          <strong>Syncing your PRs...</strong>
          <p style={{ margin: '8px 0 0', color: '#c9a83c', fontSize: '0.9em' }}>
            We&apos;re loading your team assignments and PR data. This usually takes a minute or two on first login.
          </p>
        </div>
      )}

      {/* My PRs Section */}
      <section style={{ marginBottom: '32px' }}>
        <div className="section-header">
          <h2>My PRs ({myPRs.length})</h2>
        </div>
        {isLoading && <LoadingSpinner />}
        {error && <ErrorMessage message={`Error loading PRs: ${error.message}`} />}
        {!isLoading && !error && myPRs.length === 0 && (
          <p style={{ color: '#8b949e', fontStyle: 'italic', padding: '16px 0' }}>
            No PRs authored by you
          </p>
        )}
        {!isLoading && !error && myPRs.length > 0 && (
          <PRTable prs={myPRs} showReviewColumns={showReviewColumns} showViaTeams={false} />
        )}
      </section>

      {/* PRs to Review Section */}
      <section>
        <div className="section-header">
          <h2>PRs to Review ({reviewPRs.length})</h2>
          <div style={{ display: 'flex', gap: '8px' }}>
            <button
              className="column-toggle-btn"
              onClick={handleToggleAutoReview}
              title={autoReviewEnabled ? 'Disable auto-review for requested PRs' : 'Enable auto-review for requested PRs'}
              style={{
                opacity: autoReviewEnabled ? 1 : 0.6,
                fontWeight: autoReviewEnabled ? 'bold' : 'normal'
              }}
            >
              {autoReviewEnabled ? '🤖 Auto-Review ON' : '🤖 Auto-Review OFF'}
            </button>
            {onToggleColumns && (
              <button
                className="column-toggle-btn"
                onClick={onToggleColumns}
                title={showReviewColumns ? 'Hide Status & Review columns' : 'Show Status & Review columns'}
              >
                {showReviewColumns ? '👁️ Hide AI Reviews' : '👁️ Show AI Reviews'}
              </button>
            )}
          </div>
        </div>
        {isLoading && <LoadingSpinner />}
        {error && <ErrorMessage message={`Error loading PRs: ${error.message}`} />}
        {!isLoading && !error && (
          <PRTable prs={reviewPRs} showReviewColumns={showReviewColumns} />
        )}
      </section>
    </>
  );
}
