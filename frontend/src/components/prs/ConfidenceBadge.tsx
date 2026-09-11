import { memo } from 'react';
import { CONFIDENCE_BADGES } from './confidenceBadges';
import './ConfidenceBadge.scss';

interface ConfidenceBadgeProps {
  score: number | null | undefined;
  size?: 'row' | 'large';
}

const MAX_SCORE = CONFIDENCE_BADGES.length - 1;
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

export const ConfidenceBadge = memo(function ConfidenceBadge({ score, size = 'row' }: ConfidenceBadgeProps) {
  if (score === null || score === undefined || Number.isNaN(score)) {
    return (
      <span
        className="confidence-badge confidence-badge--empty"
        role="img"
        aria-label="No merge confidence yet"
        title="No merge confidence yet"
      >
        -
      </span>
    );
  }

  const badge = CONFIDENCE_BADGES[clampScore(score)];
  return (
    <span
      className={`confidence-badge confidence-badge--${size}`}
      role="img"
      aria-label={`Merge confidence ${badge.score}/${MAX_SCORE}: ${badge.name}`}
      title={`${badge.name}: ${badge.tagline}`}
      dangerouslySetInnerHTML={{ __html: badge.svg }}
    />
  );
});
