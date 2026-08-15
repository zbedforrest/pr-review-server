import { useEffect, useSyncExternalStore } from 'react';
import { getTheme, setTheme, subscribeToTheme, type Theme } from '@/utils/theme';

export interface UseThemeResult {
  theme: Theme;
  setTheme: (theme: Theme) => void;
}

/**
 * The active theme, backed by localStorage and shared across every consumer.
 *
 * `index.html` applies the stored theme before React mounts, so this hook is
 * normally re-asserting a value the document already has; the effect matters
 * when storage was unreadable at that point, or when another tab changes it.
 */
export function useTheme(): UseThemeResult {
  const theme = useSyncExternalStore(subscribeToTheme, getTheme, getTheme);

  useEffect(() => {
    document.documentElement.setAttribute('data-theme', theme);
  }, [theme]);

  return { theme, setTheme };
}
