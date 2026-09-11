import { useCallback, useLayoutEffect, useRef } from 'react';
import { createPortal } from 'react-dom';
import { useDropdown } from '@/hooks/useDropdown';
import { ConfidenceBadge } from './ConfidenceBadge';
import { CONFIDENCE_BADGES } from './confidenceBadges';
import './ConfidenceLegend.scss';

// Keep in sync with $panel-width in ConfidenceLegend.scss.
const PANEL_WIDTH = 320;
const MAX_SCORE = CONFIDENCE_BADGES.length - 1;
const SCORING_RULE =
  'Starts at 5 with no blocking findings. Any critical finding costs 2; any medium costs 1, and three or more mediums cost 1 more; a violated required check costs 1; a request-changes verdict caps the score at 3.';

const BADGES_HIGH_TO_LOW = [...CONFIDENCE_BADGES].reverse();

/**
 * Column-header trigger for the merge-confidence legend: every medal at large
 * size with its name and rank word, plus the scoring rule.
 */
export function ConfidenceLegend() {
  const { isOpen, toggle, close, anchorRef, panelRef, position } = useDropdown({
    panelWidth: PANEL_WIDTH,
    align: 'left',
    closeOnOutsideClick: true,
    closeOnEscape: true,
  });
  const wasOpen = useRef(false);

  // The panel unmounts on close, which drops focus to body; only then does it
  // belong back on the trigger (a Tab out already landed focus somewhere).
  useLayoutEffect(() => {
    if (isOpen) {
      panelRef.current?.focus();
    } else if (wasOpen.current && document.activeElement === document.body) {
      anchorRef.current?.focus();
    }
    wasOpen.current = isOpen;
  }, [isOpen, anchorRef, panelRef]);

  const closeWhenFocusLeaves = useCallback((e: React.FocusEvent) => {
    const next = e.relatedTarget;
    if (!next || anchorRef.current?.contains(next) || panelRef.current?.contains(next)) return;
    close();
  }, [close, anchorRef, panelRef]);

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
        onBlur={isOpen ? closeWhenFocusLeaves : undefined}
      >
        Confidence
      </button>

      {isOpen && createPortal(
        <div
          ref={panelRef}
          className="confidence-legend"
          role="dialog"
          aria-label="Merge confidence legend"
          tabIndex={-1}
          style={{ top: position.top, left: position.left, width: PANEL_WIDTH, maxHeight: position.maxHeight }}
          onBlur={closeWhenFocusLeaves}
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
