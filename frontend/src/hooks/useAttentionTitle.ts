import { useEffect, useRef } from 'react';

/** Prefixes the tab title with "(N) " while N > 0 and restores the original title otherwise. */
export function useAttentionTitle(count: number) {
  const baseTitle = useRef<string | null>(null);

  useEffect(() => {
    if (baseTitle.current === null) baseTitle.current = document.title;
    const base = baseTitle.current;
    document.title = count > 0 ? `(${count}) ${base}` : base;
    return () => {
      document.title = base;
    };
  }, [count]);
}
