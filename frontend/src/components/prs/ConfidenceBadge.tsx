import { memo, useCallback, useEffect, useId, useRef, useState } from 'react';
import { createPortal } from 'react-dom';
import { useDropdown } from '@/hooks/useDropdown';
import { CONFIDENCE_BADGES } from './confidenceBadges';
import { SCORING_RULE, confidenceRecommendation } from './confidenceCopy';
import './ConfidenceBadge.scss';

interface ConfidenceBadgeProps {
  score: number | null | undefined;
  size?: 'row' | 'large';
  /** False renders the medal as a plain picture: no tooltip, no tab stop. */
  describe?: boolean;
}

const MAX_SCORE = CONFIDENCE_BADGES.length - 1;
// Shorter than the browser's own title delay, long enough that sweeping the
// pointer across the column does not flash a tooltip per row.
const HOVER_DELAY_MS = 500;
// Keep in sync with max-width in ConfidenceBadge.scss.
const TOOLTIP_WIDTH = 320;
const VIEWPORT_MARGIN = 8;
const warnedScores = new Set<number>();

function clampScore(score: number): number {
  const rounded = Math.round(score);
  if (rounded >= 0 && rounded <= MAX_SCORE) return rounded;
  if (import.meta.env.DEV && !warnedScores.has(score)) {
    warnedScores.add(score);
    console.warn(`ConfidenceBadge: score ${score} outside 0..${MAX_SCORE}, clamping`);
  }
  return Math.min(MAX_SCORE, Math.max(0, rounded));
}

function isEmpty(score: number | null | undefined): score is null | undefined {
  return score === null || score === undefined || Number.isNaN(score);
}

interface TooltipCopy {
  heading: string;
  lines: string[];
  rule?: string;
}

function tooltipCopy(score: number | null | undefined): TooltipCopy {
  if (isEmpty(score)) {
    return {
      heading: 'No merge confidence yet',
      lines: ['The score appears when the next review of this PR completes.'],
    };
  }
  const badge = CONFIDENCE_BADGES[clampScore(score)];
  return {
    heading: `Merge confidence ${badge.score}/${MAX_SCORE}: ${badge.name}`,
    lines: [confidenceRecommendation(badge.score), badge.tagline],
    rule: SCORING_RULE,
  };
}

/**
 * Hover and focus are independent reasons to show the tooltip: leaving with
 * the pointer must not close a tooltip the keyboard opened, and vice versa.
 */
function useTooltipIntent() {
  const [hovered, setHovered] = useState(false);
  const [focused, setFocused] = useState(false);
  const timer = useRef<ReturnType<typeof setTimeout>>();
  const cancel = useCallback(() => {
    if (timer.current) clearTimeout(timer.current);
    timer.current = undefined;
  }, []);

  useEffect(() => cancel, [cancel]);

  return {
    open: hovered || focused,
    onMouseEnter: useCallback(() => {
      cancel();
      timer.current = setTimeout(() => setHovered(true), HOVER_DELAY_MS);
    }, [cancel]),
    onMouseLeave: useCallback(() => {
      cancel();
      setHovered(false);
    }, [cancel]),
    onFocus: useCallback(() => setFocused(true), []),
    onBlur: useCallback(() => setFocused(false), []),
    dismiss: useCallback(() => {
      cancel();
      setHovered(false);
      setFocused(false);
    }, [cancel]),
  };
}

export const ConfidenceBadge = memo(function ConfidenceBadge({ score, size = 'row', describe = true }: ConfidenceBadgeProps) {
  const tooltipId = useId();
  const intent = useTooltipIntent();
  const { isOpen, open, close, anchorRef, panelRef, position } = useDropdown({
    panelWidth: Math.min(TOOLTIP_WIDTH, window.innerWidth - 2 * VIEWPORT_MARGIN),
    align: 'left',
    viewportMargin: VIEWPORT_MARGIN,
  });
  const showTooltip = describe && intent.open;

  useEffect(() => {
    if (showTooltip) open();
    else close();
  }, [showTooltip, open, close]);

  // Escape anywhere dismisses a hover-opened tooltip. Panels that also listen
  // on document defer to an open [role="tooltip"], so only the tooltip closes.
  const dismiss = intent.dismiss;
  useEffect(() => {
    if (!showTooltip) return;
    const onDocKeyDown = (e: KeyboardEvent) => {
      if (e.key === 'Escape') dismiss();
    };
    document.addEventListener('keydown', onDocKeyDown);
    return () => document.removeEventListener('keydown', onDocKeyDown);
  }, [showTooltip, dismiss]);

  const onKeyDown = useCallback(
    (e: React.KeyboardEvent) => {
      if (e.key !== 'Escape' || !intent.open) return;
      // Escape is spent on the tooltip; enclosing panels must not close too.
      e.stopPropagation();
      intent.dismiss();
    },
    [intent]
  );

  const copy = tooltipCopy(score);
  const badge = isEmpty(score) ? null : CONFIDENCE_BADGES[clampScore(score)];
  const ariaLabel = badge
    ? `Merge confidence ${badge.score}/${MAX_SCORE}, ${badge.name}. ${confidenceRecommendation(badge.score)}`
    : 'No merge confidence yet';

  const interactive = describe
    ? {
        tabIndex: 0,
        'aria-describedby': isOpen ? tooltipId : undefined,
        onMouseEnter: intent.onMouseEnter,
        onMouseLeave: intent.onMouseLeave,
        onFocus: intent.onFocus,
        onBlur: intent.onBlur,
        onKeyDown,
      }
    : {};

  const tooltip = isOpen
    ? createPortal(
        <div
          id={tooltipId}
          ref={panelRef}
          role="tooltip"
          className="confidence-tooltip"
          style={{ top: position.top, left: position.left, maxHeight: position.maxHeight }}
        >
          <p className="confidence-tooltip__heading">{copy.heading}</p>
          {copy.lines.map((line) => (
            <p key={line} className="confidence-tooltip__line">
              {line}
            </p>
          ))}
          {copy.rule && <p className="confidence-tooltip__rule">{copy.rule}</p>}
        </div>,
        document.body
      )
    : null;

  const spanRef = anchorRef as React.RefObject<HTMLSpanElement>;

  if (!badge) {
    return (
      <>
        <span ref={spanRef} role="img" aria-label={ariaLabel} className="confidence-badge confidence-badge--empty" {...interactive}>
          -
        </span>
        {tooltip}
      </>
    );
  }

  return (
    <>
      <span
        ref={spanRef}
        role="img"
        aria-label={ariaLabel}
        className={`confidence-badge confidence-badge--${size}`}
        dangerouslySetInnerHTML={{ __html: badge.svg }}
        {...interactive}
      />
      {tooltip}
    </>
  );
});
