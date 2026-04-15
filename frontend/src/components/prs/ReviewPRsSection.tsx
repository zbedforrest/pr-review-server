import { useEffect } from 'react';
import { usePRs } from '@/hooks/usePRs';
import { useCurrentUser } from '@/hooks/useCurrentUser';
import { useTelemetry } from '@/hooks/useTelemetry';
import { PRTable } from './PRTable';
import { LoadingSpinner, ErrorMessage } from '@/components/common';
import { PR } from '@/types/pr';
import { VIA_TEAMS_PERSONAL } from '@/constants';
import { TriageFilter, categorizePR } from './triageUtils';
import { AutoReviewToggle } from './AutoReviewToggle';
import './SectionHeader.scss';
import { FilterBar } from '../filters';

interface ReviewPRsSectionProps {
  showReviewColumns?: boolean;
  onToggleColumns?: () => void;
  searchTerm?: string;
  triageFilter?: TriageFilter | null;
  selectedTeams?: string[];
  selectedRepos?: string[];
  onTeamsChange?: (teams: string[]) => void;
  onReposChange?: (repos: string[]) => void;
}

interface FilterOptions {
  searchTerm: string;
  selectedTeams?: string[];
  selectedRepos?: string[];
  username?: string;
}

function getTeamFilterName(team: string, username?: string): string {
  if (team === VIA_TEAMS_PERSONAL) {
    return username ? `@${username}` : '@you';
  }

  const suffixIndex = team.lastIndexOf(':');
  return suffixIndex === -1 ? team : team.slice(0, suffixIndex);
}

function filterAndSortPRs(prs: PR[], { searchTerm, selectedTeams, selectedRepos, username }: FilterOptions): PR[] {
  return prs
    .filter((pr) => {
      if (searchTerm) {
        const lowerTerm = searchTerm.toLowerCase();
        const matchesSearch =
          pr.title.toLowerCase().includes(lowerTerm) ||
          pr.repo.toLowerCase().includes(lowerTerm) ||
          pr.owner.toLowerCase().includes(lowerTerm) ||
          pr.author.toLowerCase().includes(lowerTerm) ||
          pr.number.toString().includes(lowerTerm);
        if (!matchesSearch) return false;
      }

      if (selectedTeams && selectedTeams.length > 0) {
        if (!pr.via_teams.some(t => selectedTeams.includes(getTeamFilterName(t, username)))) return false;
      }

      if (selectedRepos && selectedRepos.length > 0) {
        if (!selectedRepos.includes(`${pr.owner}/${pr.repo}`)) return false;
      }

      return true;
    })
    .sort((a, b) => {
      if (!a.created_at && !b.created_at) return 0;
      if (!a.created_at) return 1;
      if (!b.created_at) return -1;
      return new Date(b.created_at).getTime() - new Date(a.created_at).getTime();
    });
}

export function ReviewPRsSection({
  showReviewColumns = true,
  onToggleColumns,
  searchTerm = '',
  triageFilter = null,
  selectedTeams = [],
  selectedRepos = [],
  onTeamsChange,
  onReposChange,
}: ReviewPRsSectionProps) {
  const { data: prs, isLoading, error } = usePRs();
  const { data: currentUser } = useCurrentUser();
  const { track } = useTelemetry();

  useEffect(() => {
    track('view_review_prs_page');
  }, [track]);

  const filterOpts: FilterOptions = {
    searchTerm,
    selectedTeams,
    selectedRepos,
    username: currentUser?.github_username,
  };

  // Split PRs into "My PRs" and "PRs to Review"
  const allPRs = prs || [];
  const myPRs = filterAndSortPRs(allPRs.filter(pr => pr.is_mine), filterOpts);

  // Apply triage filter to review PRs
  let reviewPRs = filterAndSortPRs(allPRs.filter(pr => !pr.is_mine), filterOpts);
  if (triageFilter) {
    reviewPRs = reviewPRs.filter(pr => {
      // Only filter completed PRs by triage category
      if (pr.status !== 'completed') return true;
      return categorizePR(pr) === triageFilter;
    });
  }

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
          <div className="section-header__actions">
            <AutoReviewToggle />
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
        {onTeamsChange && onReposChange && (
          <FilterBar
            selectedTeams={selectedTeams}
            onTeamsChange={onTeamsChange}
            selectedRepos={selectedRepos}
            onReposChange={onReposChange}
          />
        )}
        {isLoading && <LoadingSpinner />}
        {error && <ErrorMessage message={`Error loading PRs: ${error.message}`} />}
        {!isLoading && !error && (
          <PRTable prs={reviewPRs} showReviewColumns={showReviewColumns} />
        )}
      </section>
    </>
  );
}
