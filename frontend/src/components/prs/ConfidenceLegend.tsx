import { createPortal } from 'react-dom';
import { useDropdown } from '@/hooks/useDropdown';
import { ConfidenceBadge } from './ConfidenceBadge';
import { CONFIDENCE_BADGES } from './confidenceBadges';
import './ConfidenceLegend.scss';

// Keep in sync with $panel-width in ConfidenceLegend.scss.
const PANEL_WIDTH = 320;
const MAX_SCORE = CONFIDENCE_BADGES.length - 1;
const SCORING_RULE =
  '5 no blocking findings; a critical costs 2; a medium costs 1; three or more mediums cost 1 more; a violated required check costs 1; a request-changes verdict caps at 3';

const BADGES_HIGH_TO_LOW = [...CONFIDENCE_BADGES].reverse();

/**
 * Column-header trigger for the merge-confidence legend: every medal at large
 * size with its name and rank word, plus the scoring rule.
 */
export function ConfidenceLegend() {
  const { isOpen, toggle, anchorRef, panelRef, position } = useDropdown({
    panelWidth: PANEL_WIDTH,
    align: 'left',
    closeOnOutsideClick: true,
    closeOnEscape: true,
  });

  return (
    <>
      <button
        type="button"
        ref={anchorRef as React.RefObject<HTMLButtonElement>}
        className="confidence-legend__trigger"
        aria-haspopup="dialog"
        aria-expanded={isOpen}
        title="How merge confidence is scored"
        onClick={toggle}
      >
        Confidence
      </button>

      {isOpen && createPortal(
        <div
          ref={panelRef}
          className="confidence-legend"
          role="dialog"
          aria-label="Merge confidence legend"
          style={{ top: position.top, left: position.left, width: PANEL_WIDTH }}
        >
          <ul className="confidence-legend__list">
            {BADGES_HIGH_TO_LOW.map((badge) => (
              <li key={badge.score} className="confidence-legend__item">
                <ConfidenceBadge score={badge.score} size="large" />
                <span className="confidence-legend__score">{badge.score}/{MAX_SCORE}</span>
                <span className="confidence-legend__name">{badge.name}</span>
                <span className="confidence-legend__rank">{badge.rankWord}</span>
              </li>
            ))}
          </ul>
          <p className="confidence-legend__rule">{SCORING_RULE}</p>
        </div>,
        document.body
      )}
    </>
  );
}
