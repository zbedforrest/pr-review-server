import { useTheme } from '@/hooks/useTheme';
import { useTelemetry } from '@/hooks/useTelemetry';
import { isTheme, THEME_OPTIONS } from '@/utils/theme';
import './ThemeSelector.scss';

/**
 * Theme picker for the header. A native <select> rather than a custom menu:
 * twenty options get keyboard navigation, type-ahead and mobile pickers for
 * free, and the list is plain text.
 */
export function ThemeSelector() {
  const { theme, setTheme } = useTheme();
  const { track } = useTelemetry();

  return (
    <label className="theme-selector">
      <span className="theme-selector__label">Theme</span>
      <select
        className="theme-selector__select"
        aria-label="Theme"
        value={theme}
        onChange={(e) => {
          const next = e.target.value;
          if (!isTheme(next)) return;
          setTheme(next);
          track('theme_change', { label: next });
        }}
      >
        {THEME_OPTIONS.map((option) => (
          <option key={option.value} value={option.value}>
            {option.label}
          </option>
        ))}
      </select>
    </label>
  );
}
