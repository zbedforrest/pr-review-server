import { useEffect, useMemo, useState } from 'react';
import { usePRs } from '@/hooks/usePRs';
import { useCurrentUser } from '@/hooks/useCurrentUser';
import { useTelemetry } from '@/hooks/useTelemetry';
import { useSectionsConfig } from '@/hooks/useSectionsConfig';
import { PRTable } from './PRTable';
import { PRSection } from './PRSection';
import { LoadingSpinner, ErrorMessage } from '@/components/common';
import { PR } from '@/types/pr';
import { getTeamFilterName } from '@/utils/teamFilters';
import { filterAndSortPRs, prKey, prMatchesFilters } from '@/utils/sectionFilters';
import { TriageFilter, categorizePR } from './triageUtils';
import type { PRStateFilter } from '@/hooks/useUrlFilters';
import './SectionHeader.scss';
import './PRSections.scss';

interface ReviewPRsSectionProps {
  searchTerm?: string;
  triageFilter?: TriageFilter | null;
  selectedTeams?: string[];
  selectedRepos?: string[];
  selectedStates?: PRStateFilter[];
  /** prKey()s already shown elsewhere on the page (the re-review pin above the filters). */
  excludeKeys?: ReadonlySet<string>;
}

export function ReviewPRsSection({
  searchTerm = '',
  triageFilter = null,
  selectedTeams = [],
  selectedRepos = [],
  selectedStates = [],
  excludeKeys,
}: ReviewPRsSectionProps) {
  const { data: prs, isLoading, error } = usePRs();
  const { data: currentUser } = useCurrentUser();
  const { track } = useTelemetry();
  const {
    sections,
    showUncategorized,
    addSection,
    removeSection,
    renameSection,
    setSectionFilters,
    setSectionCollapsed,
    duplicateSection,
    moveSection,
    setShowUncategorized,
    resetSections,
  } = useSectionsConfig();

  // Collapse state for the two built-in sections is deliberately client-side
  // only (per issue #30); hidden membership itself is server-persisted per
  // user. Requested starts open (it's the section the user explicitly
  // populated), Hidden starts closed. Configured sections persist their own
  // collapse state alongside the rest of their config.
  const [hiddenExpanded, setHiddenExpanded] = useState(false);
  const [requestedExpanded, setRequestedExpanded] = useState(true);
  const [customizing, setCustomizing] = useState(false);
  const [editingId, setEditingId] = useState<string | null>(null);
  const [dragIndex, setDragIndex] = useState<number | null>(null);

  useEffect(() => {
    track('view_review_prs_page');
  }, [track]);

  const username = currentUser?.github_username;
  const allPRs = useMemo(() => prs || [], [prs]);

  // Options offered by the section editor, derived from the loaded PRs the
  // same way the global filter bar derives its own.
  const availableTeams = useMemo(
    () =>
      Array.from(
        new Set(allPRs.flatMap((pr) => pr.via_teams.map((team) => getTeamFilterName(team, username))))
      ).sort(),
    [allPRs, username]
  );
  const availableRepos = useMemo(
    () => Array.from(new Set(allPRs.map((pr) => `${pr.owner}/${pr.repo}`))).sort(),
    [allPRs]
  );
  const availableAuthors = useMemo(
    () => Array.from(new Set(allPRs.map((pr) => pr.author).filter(Boolean))).sort(),
    [allPRs]
  );

  // The two built-in sections keep their fixed membership: hidden > via_manual
  // wins over anything the user configures, so a manually requested PR still
  // lands in exactly one place and hidden rows stay out of the way.
  const sectionPool = excludeKeys ? allPRs.filter((pr) => !excludeKeys.has(prKey(pr))) : allPRs;
  const visiblePRs = sectionPool.filter((pr) => !pr.hidden);
  const hasHiddenPRs = sectionPool.length > visiblePRs.length;
  const hasRequestedPRs = visiblePRs.some((pr) => pr.via_manual);

  // The filter bar's criteria apply on top of every section's own filters.
  const globalCriteria = {
    search: searchTerm,
    teams: selectedTeams,
    repos: selectedRepos,
    states: selectedStates,
  };
  const globallyFiltered = filterAndSortPRs(
    visiblePRs.filter((pr) => !pr.via_manual),
    globalCriteria,
    username
  );
  const requestedPRs = filterAndSortPRs(
    visiblePRs.filter((pr) => pr.via_manual),
    globalCriteria,
    username
  );
  const hiddenPRs = filterAndSortPRs(
    sectionPool.filter((pr) => pr.hidden),
    globalCriteria,
    username
  );

  const applyTriage = (sectionPRs: PR[]): PR[] => {
    if (!triageFilter) return sectionPRs;
    return sectionPRs.filter((pr) => {
      // Triage is a review-queue concern, so the user's own PRs are exempt
      // wherever they appear — the same carve-out the fixed "My PRs" section
      // used to get.
      if (pr.is_mine) return true;
      // Only filter completed PRs by triage category
      if (pr.status !== 'completed') return true;
      return categorizePR(pr) === triageFilter;
    });
  };

  const matchedKeys = new Set<string>();
  const sectionPRs = sections.map((section) => {
    const matches = globallyFiltered.filter((pr) => prMatchesFilters(pr, section.filters, username));
    for (const pr of matches) matchedKeys.add(prKey(pr));
    return applyTriage(matches);
  });

  const uncategorizedPRs = applyTriage(
    globallyFiltered.filter((pr) => !matchedKeys.has(prKey(pr)))
  );

  const showSyncBanner = !isLoading && !error && allPRs.length === 0;

  const handleDrop = (targetIndex: number) => {
    if (dragIndex === null || dragIndex === targetIndex) return;
    track('move_section', { label: `${dragIndex}->${targetIndex}` });
    moveSection(dragIndex, targetIndex);
    setDragIndex(null);
  };

  return (
    <>
      {showSyncBanner && (
        <div className="review-prs__sync-banner">
          <strong>Syncing your PRs...</strong>
          <p className="review-prs__sync-banner-copy">
            We&apos;re loading your team assignments and PR data. This usually takes a minute or two on first login.
          </p>
        </div>
      )}

      <div className="section-toolbar">
        <button
          type="button"
          className="column-toggle-btn"
          aria-pressed={customizing}
          onClick={() => {
            const next = !customizing;
            track('customize_sections', { label: next ? 'open' : 'close' });
            setCustomizing(next);
            if (!next) setEditingId(null);
          }}
        >
          {customizing ? 'Done customizing' : 'Customize sections'}
        </button>

        {customizing && (
          <>
            <button
              type="button"
              className="column-toggle-btn"
              onClick={() => {
                const section = addSection();
                track('add_section');
                setEditingId(section.id);
              }}
            >
              + Add section
            </button>
            <button
              type="button"
              className="column-toggle-btn"
              onClick={() => {
                track('reset_sections');
                setEditingId(null);
                resetSections();
              }}
            >
              Reset to defaults
            </button>
            <label className="section-toolbar__checkbox">
              <input
                type="checkbox"
                checked={showUncategorized}
                onChange={(e) => setShowUncategorized(e.target.checked)}
              />
              Show unmatched PRs
            </label>
          </>
        )}
      </div>

      {isLoading && <LoadingSpinner />}
      {error && <ErrorMessage message={`Error loading PRs: ${error.message}`} />}

      {/* Requested by Me Section — PRs the user manually requested a review
          for (paste-a-URL form / API). Rendered only when non-empty; deleting
          an entry removes it and the section disappears with its last row.
          It sits outside the configurable sections on purpose: these rows are
          an explicit user action, not something to filter into a queue. */}
      {hasRequestedPRs && (
        <section className="review-prs__requested-section">
          <div className="section-header">
            <h2>
              <button
                type="button"
                className="hidden-section__toggle"
                aria-expanded={requestedExpanded}
                onClick={() => setRequestedExpanded(v => !v)}
              >
                <span className="hidden-section__chevron" aria-hidden="true">
                  {requestedExpanded ? '▾' : '▸'}
                </span>
                Requested by Me ({requestedPRs.length})
              </button>
            </h2>
          </div>
          {requestedExpanded && requestedPRs.length === 0 && (
            <p className="review-prs__empty-state">
              No matching requested PRs
            </p>
          )}
          {requestedExpanded && requestedPRs.length > 0 && (
            <PRTable prs={requestedPRs} showViaTeams={false} />
          )}
        </section>
      )}

      {sections.map((section, index) => (
        <PRSection
          key={section.id}
          section={section}
          prs={sectionPRs[index]}
          index={index}
          sectionCount={sections.length}
          customizing={customizing}
          editing={editingId === section.id}
          availableTeams={availableTeams}
          availableRepos={availableRepos}
          availableAuthors={availableAuthors}
          dragging={dragIndex === index}
          onToggleCollapsed={() => setSectionCollapsed(section.id, !section.collapsed)}
          onToggleEditing={() => setEditingId(editingId === section.id ? null : section.id)}
          onTitleChange={(title) => renameSection(section.id, title)}
          onFiltersChange={(filters) => setSectionFilters(section.id, filters)}
          onDuplicate={() => {
            track('duplicate_section');
            duplicateSection(section.id);
          }}
          onRemove={() => {
            track('remove_section');
            if (editingId === section.id) setEditingId(null);
            removeSection(section.id);
          }}
          onMove={(from, to) => {
            track('move_section', { label: `${from}->${to}` });
            moveSection(from, to);
          }}
          onDragStart={setDragIndex}
          onDragEnd={() => setDragIndex(null)}
          onDropOn={handleDrop}
        />
      ))}

      {/* Anything the configured sections miss still gets a home, so narrowing
          a section's filters never makes a PR vanish from the page. */}
      {showUncategorized && uncategorizedPRs.length > 0 && (
        <section className="pr-section">
          <div className="section-header">
            <h2 className="pr-section__heading">
              <span className="pr-section__title" data-testid="section-title">
                Other PRs ({uncategorizedPRs.length})
              </span>
            </h2>
          </div>
          <PRTable prs={uncategorizedPRs} />
        </section>
      )}

      {/* Hidden Section — collapsed by default, only shown once the user has
          hidden something. Search/team/repo filters apply with the same
          semantics as the sections above (the count reflects matches). */}
      {hasHiddenPRs && (
        <section className="review-prs__hidden-section">
          <div className="section-header">
            <h2>
              <button
                type="button"
                className="hidden-section__toggle"
                aria-expanded={hiddenExpanded}
                onClick={() => setHiddenExpanded((v) => !v)}
              >
                <span className="hidden-section__chevron" aria-hidden="true">
                  {hiddenExpanded ? '▾' : '▸'}
                </span>
                Hidden ({hiddenPRs.length})
              </button>
            </h2>
          </div>
          {hiddenExpanded && <PRTable prs={hiddenPRs} />}
        </section>
      )}
    </>
  );
}
