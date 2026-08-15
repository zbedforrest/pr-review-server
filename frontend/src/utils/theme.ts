/**
 * Theme selection.
 *
 * A theme is nothing more than a `data-theme` value on <html>; the palettes in
 * `styles/themes/_palettes.scss` do the rest. The choice is a per-browser
 * preference, so it lives in localStorage rather than on the server — the same
 * arrangement as the section layout.
 */

export const VALID_THEMES = [
  'light',
  'dark',
  'github-light',
  'github-dark',
  'gruvbox-dark',
  'gruvbox-light',
  'solarized-light',
  'solarized-dark',
  'monokai',
  'dracula',
  'nord',
  'night-owl',
  'tokyo-night',
  'catppuccin-latte',
  'catppuccin-frappe',
  'catppuccin-macchiato',
  'catppuccin-mocha',
  'everforest',
  'rose-pine',
  'synthwave-84',
] as const;

export type Theme = (typeof VALID_THEMES)[number];

export interface ThemeOption {
  value: Theme;
  label: string;
}

/** Menu order: the two defaults first, then the rest grouped by family. */
export const THEME_OPTIONS: ThemeOption[] = [
  { value: 'dark', label: '🌙 Dark (One Dark)' },
  { value: 'light', label: '☀️ Light (One Light)' },
  { value: 'github-dark', label: '🐙 GitHub Dark' },
  { value: 'github-light', label: '🐙 GitHub Light' },
  { value: 'gruvbox-dark', label: '📦 Gruvbox Dark' },
  { value: 'gruvbox-light', label: '📦 Gruvbox Light' },
  { value: 'solarized-dark', label: '☀️ Solarized Dark' },
  { value: 'solarized-light', label: '☀️ Solarized Light' },
  { value: 'monokai', label: '🍬 Monokai' },
  { value: 'dracula', label: '🧛 Dracula' },
  { value: 'nord', label: '❄️ Nord' },
  { value: 'night-owl', label: '🦉 Night Owl' },
  { value: 'tokyo-night', label: '🌃 Tokyo Night' },
  { value: 'catppuccin-mocha', label: '🐱 Catppuccin Mocha' },
  { value: 'catppuccin-macchiato', label: '🐱 Catppuccin Macchiato' },
  { value: 'catppuccin-frappe', label: '🐱 Catppuccin Frappé' },
  { value: 'catppuccin-latte', label: '🐱 Catppuccin Latte' },
  { value: 'everforest', label: '🌲 Everforest' },
  { value: 'rose-pine', label: '🌹 Rose Pine' },
  { value: 'synthwave-84', label: "🕹️ SynthWave '84" },
];

/**
 * Theme choice is a per-browser preference. The key carries a version so a
 * future incompatible value gets a new key instead of confusing an old client.
 *
 * Kept in sync by hand with the pre-paint script in `index.html`, which reads
 * the same key before React mounts. `theme.test.ts` asserts they still match.
 */
export const THEME_STORAGE_KEY = 'prism.theme.v1';

/** Used when nothing is stored and the OS expresses no preference. */
export const DEFAULT_THEME: Theme = 'dark';

/**
 * Themes that have been renamed. A value persisted by an older client maps to
 * its current equivalent instead of silently falling back to the default.
 */
const LEGACY_THEME_ALIASES: Record<string, Theme> = {
  // 'catppuccin' shipped as Catppuccin's Mocha flavor before the other three
  // flavors were added.
  catppuccin: 'catppuccin-mocha',
};

export const isTheme = (value: unknown): value is Theme =>
  typeof value === 'string' && (VALID_THEMES as readonly string[]).includes(value);

/**
 * Normalizes a stored theme value, returning null when it names no theme this
 * build knows about so the caller can fall back to its own default.
 */
export function resolveTheme(value: string | null): Theme | null {
  if (isTheme(value)) return value;
  // hasOwnProperty rather than `in`: the latter walks the prototype chain, so a
  // stored value of 'constructor' or 'toString' would "resolve" to a function.
  if (value !== null && Object.prototype.hasOwnProperty.call(LEGACY_THEME_ALIASES, value)) {
    return LEGACY_THEME_ALIASES[value];
  }
  return null;
}

/** The theme matching the OS-level light/dark preference. */
export function systemTheme(): Theme {
  return window.matchMedia?.('(prefers-color-scheme: light)').matches ? 'light' : DEFAULT_THEME;
}

export function loadTheme(): Theme {
  try {
    const stored = resolveTheme(window.localStorage.getItem(THEME_STORAGE_KEY));
    if (stored) return stored;
  } catch {
    // Storage blocked entirely (private mode, cookies disabled). The OS
    // preference still gives a sensible starting point.
  }
  return systemTheme();
}

export function saveTheme(theme: Theme): void {
  try {
    window.localStorage.setItem(THEME_STORAGE_KEY, theme);
  } catch {
    // Storage full or unavailable: the theme still applies for this session,
    // it just won't survive a reload.
  }
}

/** Publishes the theme to the document, which is what actually repaints. */
export function applyTheme(theme: Theme): void {
  document.documentElement.setAttribute('data-theme', theme);
}

// --- Store -----------------------------------------------------------------
// A module-level store rather than component state so every consumer (the
// selector in the header, anything added later) sees the same value, without
// threading a provider through the tree.

let currentTheme: Theme | null = null;
const listeners = new Set<() => void>();

export function getTheme(): Theme {
  if (currentTheme === null) currentTheme = loadTheme();
  return currentTheme;
}

export function setTheme(theme: Theme): void {
  if (theme === currentTheme) return;
  currentTheme = theme;
  saveTheme(theme);
  applyTheme(theme);
  listeners.forEach((listener) => listener());
}

/** Re-reads storage and adopts the result. Used for cross-tab updates. */
function syncFromStorage(): void {
  const next = loadTheme();
  if (next === currentTheme) return;
  currentTheme = next;
  applyTheme(next);
  listeners.forEach((listener) => listener());
}

function onStorageEvent(event: StorageEvent): void {
  // A null key means the whole store was cleared, which may have dropped ours.
  if (event.key !== null && event.key !== THEME_STORAGE_KEY) return;
  syncFromStorage();
}

/**
 * Subscribes to theme changes, including ones made in another tab. The
 * `storage` listener is attached only while someone is subscribed.
 */
export function subscribeToTheme(listener: () => void): () => void {
  if (listeners.size === 0) window.addEventListener('storage', onStorageEvent);
  listeners.add(listener);
  return () => {
    listeners.delete(listener);
    if (listeners.size === 0) window.removeEventListener('storage', onStorageEvent);
  };
}

/** Test seam: drops the cached theme so the next read hits storage again. */
export function resetThemeStore(): void {
  currentTheme = null;
}
