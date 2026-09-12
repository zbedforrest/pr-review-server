export const SCORING_RULE =
  'Starts at 5 with no blocking findings. Any critical finding costs 2; any medium costs 1, and three or more mediums cost 1 more; a violated required check costs 1; a request-changes verdict caps the score at 3.';

// Mirrors the publisher's recommendation line so the tooltip says exactly what
// the sticky GitHub comment says for the same score.
export function confidenceRecommendation(score: number): string {
  if (score >= 5) return 'No blocking findings.';
  if (score === 4) return 'Minor findings worth a look before merge.';
  if (score === 3) return 'Findings that should be addressed before merge.';
  return 'Significant findings; please address before merge.';
}
