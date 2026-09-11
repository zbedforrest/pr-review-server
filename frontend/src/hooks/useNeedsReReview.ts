import { useMemo } from 'react';
import { usePRs } from '@/hooks/usePRs';
import type { PR } from '@/types/pr';
import { prKey } from '@/utils/sectionFilters';

/** Oldest first, with PRs of unknown creation date sorted last. */
function sortPRsByOldest(prs: PR[]): PR[] {
  return [...prs].sort((a, b) => {
    if (!a.created_at && !b.created_at) return 0;
    if (!a.created_at) return 1;
    if (!b.created_at) return -1;
    return new Date(a.created_at).getTime() - new Date(b.created_at).getTime();
  });
}

export function useNeedsReReview() {
  const { data: prs } = usePRs();

  return useMemo(() => {
    const rows = sortPRsByOldest((prs || []).filter((pr) => pr.needs_attention && !pr.hidden));
    return { rows, keys: new Set(rows.map(prKey)), count: rows.length };
  }, [prs]);
}
