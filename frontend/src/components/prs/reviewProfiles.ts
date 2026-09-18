import type { ReviewProfile } from '@/types/pr';

export const PROFILE_LABELS: Record<ReviewProfile, string> = {
  full: 'Full (heavy)',
  lite: 'Lite',
  lite_plus: 'Lite+',
};

export const PROFILE_CHOICES: { profile: ReviewProfile; title: string; description: string }[] = [
  { profile: 'lite', title: 'Lite review', description: 'One agent over the inlined diff; about two minutes' },
  { profile: 'lite_plus', title: 'Lite+ review', description: 'Lite with sub-agents for larger changes; up to ten minutes' },
  { profile: 'full', title: 'Full (heavy) review', description: 'First pass, gates and agent; the complete pipeline' },
];

export const formatCost = (usd: number) => `$${usd.toFixed(2)}`;
