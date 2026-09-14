import { memo, useCallback, useEffect, useId, useRef, useState } from 'react';
import { createPortal } from 'react-dom';
import { CONFIDENCE_BADGES } from './confidenceBadges';
import { SCORING_RULE, confidenceRecommendation } from './confidenceCopy';
import './ConfidenceBadge.scss';

interface ConfidenceBadgeProps {
  score: number | null | undefined;
  size?: 'row' | 'large';
}

const MAX_SCORE = CONFIDENCE_BADGES.length - 1;
// Shorter than the browser's own title delay, long enough that sweeping the
// pointer across the column does not flash a tooltip per row.
const HOVER_DELAY_MS = 500;
const TOOLTIP_GAP = 8;
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

interface TooltipCopy {
  heading: string;
  lines: string[];
  rule?: string;
}

function tooltipCopy(score: number | null | undefined): TooltipCopy {
  if (score === null || score === undefined || Number.isNaN(score)) {
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

function useHoverTooltip() {
  const [open, setOpen] = useState(false);
  const timer = useRef<ReturnType<typeof setTimeout>>();
  const cancel = useCallback(() => {
    if (timer.current) clearTimeout(timer.current);
    timer.current = undefined;
  }, []);
  const show = useCallback(() => setOpen(true), []);
  const hide = useCallback(() => {
    cancel();
    setOpen(false);
  }, [cancel]);
  const showAfterDelay = useCallback(() => {
    cancel();
    timer.current = setTimeout(show, HOVER_DELAY_MS);
  }, [cancel, show]);

  useEffect(() => cancel, [cancel]);
  useEffect(() => {
    if (!open) return;
    const onKeyDown = (e: KeyboardEvent) => {
      if (e.key === 'Escape') hide();
    };
    document.addEventListener('keydown', onKeyDown);
    return () => document.removeEventListener('keydown', onKeyDown);
  }, [open, hide]);

  return { open, show, hide, showAfterDelay };
}

// Below the badge unless that would run off the viewport; the horizontal
// centre is clamped so a badge near the right edge does not push the panel
// off-screen (the panel's max width is 320px).
const TOOLTIP_HALF_WIDTH = 160;
const TOOLTIP_EST_HEIGHT = 200;

function tooltipStyle(anchor: HTMLElement | null): React.CSSProperties {
  if (!anchor) return {};
  const rect = anchor.getBoundingClientRect();
  const left = Math.min(
    Math.max(rect.left + rect.width / 2, TOOLTIP_HALF_WIDTH + TOOLTIP_GAP),
    window.innerWidth - TOOLTIP_HALF_WIDTH - TOOLTIP_GAP
  );
  if (rect.bottom + TOOLTIP_GAP + TOOLTIP_EST_HEIGHT > window.innerHeight) {
    return { bottom: window.innerHeight - rect.top + TOOLTIP_GAP, left };
  }
  return { top: rect.bottom + TOOLTIP_GAP, left };
}

export const ConfidenceBadge = memo(function ConfidenceBadge({ score, size = 'row' }: ConfidenceBadgeProps) {
  const anchorRef = useRef<HTMLSpanElement>(null);
  const tooltipId = useId();
  const { open, show, hide, showAfterDelay } = useHoverTooltip();
  const copy = tooltipCopy(score);
  const empty = score === null || score === undefined || Number.isNaN(score);
  const badge = empty ? null : CONFIDENCE_BADGES[clampScore(score as number)];

  const tooltip = open
    ? createPortal(
        <div id={tooltipId} role="tooltip" className="confidence-tooltip" style={tooltipStyle(anchorRef.current)}>
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

  const shared = {
    ref: anchorRef,
    role: 'img',
    tabIndex: 0,
    'aria-describedby': open ? tooltipId : undefined,
    onMouseEnter: showAfterDelay,
    onMouseLeave: hide,
    onFocus: show,
    onBlur: hide,
  };

  if (!badge) {
    return (
      <>
        <span {...shared} className="confidence-badge confidence-badge--empty" aria-label="No merge confidence yet">
          -
        </span>
        {tooltip}
      </>
    );
  }

  return (
    <>
      <span
        {...shared}
        className={`confidence-badge confidence-badge--${size}`}
        aria-label={`${copy.heading.replace(': ', ', ')}. ${confidenceRecommendation(badge.score)}`}
        dangerouslySetInnerHTML={{ __html: badge.svg }}
      />
      {tooltip}
    </>
  );
});
