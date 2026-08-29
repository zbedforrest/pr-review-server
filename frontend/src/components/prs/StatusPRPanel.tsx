import { useEffect, useMemo, useRef } from 'react';
import { usePRs } from '@/hooks/usePRs';
import { sortPRsByNewest } from '@/utils/sectionFilters';
import type { StatusPanelFilter } from '@/types/status';
import { PRTable } from './PRTable';
import './SectionHeader.scss';
import './PRSections.scss';
import './StatusPRPanel.scss';

const STATUS_LABELS: Record<StatusPanelFilter, string> = {
  completed: 'Completed',
  generating: 'Generating',
};

interface StatusPRPanelProps {
  status: StatusPanelFilter;
  onClose: () => void;
  /**
   * The count the user clicked in the status bar. It's server-wide (every PR
   * the poller tracks) while the table can only show PRs in this user's own
   * list, so the two disagree often enough to be worth spelling out.
   */
  serverCount?: number;
}

/**
 * A peek at every PR sitting in one review status, opened by clicking that
 * count in the status bar. Uses the same table as the configured sections but
 * is hard-filtered on status alone — the page's search/team/repo filters and
 * the Hidden section deliberately don't narrow it.
 */
export function StatusPRPanel({ status, onClose, serverCount }: StatusPRPanelProps) {
  const { data: prs } = usePRs();

  // Read through a ref so the listener below can register once per mount. A
  // fresh onClose identity would otherwise re-register it on every parent
  // render, moving it behind the row menus' own Escape listeners.
  const onCloseRef = useRef(onClose);
  useEffect(() => {
    onCloseRef.current = onClose;
  }, [onClose]);

  // Escape closes the panel from anywhere on the page, except while a row menu
  // is open: that menu also listens on document, and since it opened after the
  // panel it runs second, so bailing here lets Escape dismiss just the menu.
  useEffect(() => {
    const onKeyDown = (e: KeyboardEvent) => {
      if (e.key !== 'Escape') return;
      if (document.querySelector('[role="menu"]')) return;
      onCloseRef.current();
    };
    document.addEventListener('keydown', onKeyDown);
    return () => document.removeEventListener('keydown', onKeyDown);
  }, []);

  const matching = useMemo(
    () => sortPRsByNewest((prs || []).filter((pr) => pr.status === status)),
    [prs, status]
  );

  const label = STATUS_LABELS[status];
  const showServerCount = serverCount !== undefined && serverCount !== matching.length;

  return (
    <section
      className="status-pr-panel"
      aria-label={`${label} PRs`}
      data-testid="status-pr-panel"
    >
      <div className="section-header">
        <h2 className="pr-section__heading">
          <span className="pr-section__title" data-testid="section-title">
            {label} ({matching.length})
          </span>
        </h2>

        <div className="section-header__actions">
          {showServerCount && (
            <span className="status-pr-panel__note">
              {matching.length} of {serverCount} server-wide
            </span>
          )}
          <span className="status-pr-panel__hint" aria-hidden="true">
            Esc to close
          </span>
          <button
            type="button"
            className="column-toggle-btn"
            aria-label={`Close ${label} PRs`}
            onClick={onClose}
          >
            ✕
          </button>
        </div>
      </div>

      {matching.length === 0 ? (
        <p className="review-prs__empty-state">No {label.toLowerCase()} PRs in your list</p>
      ) : (
        <PRTable prs={matching} />
      )}
    </section>
  );
}
