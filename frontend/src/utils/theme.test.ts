import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
// Imported as text rather than read through node:fs so the test stays inside
// the app's module graph — no @types/node, and the paths break at build time
// rather than at run time if a file moves.
import palettesScss from '@/styles/themes/_palettes.scss?raw';
import indexHtml from '../../index.html?raw';
import {
  DEFAULT_THEME,
  getTheme,
  isTheme,
  loadTheme,
  resetThemeStore,
  resolveTheme,
  saveTheme,
  setTheme,
  subscribeToTheme,
  THEME_OPTIONS,
  THEME_STORAGE_KEY,
  VALID_THEMES,
} from './theme';

describe('isTheme', () => {
  it('accepts every declared theme', () => {
    for (const theme of VALID_THEMES) {
      expect(isTheme(theme)).toBe(true);
    }
  });

  it('rejects unknown values and non-strings', () => {
    expect(isTheme('vaporwave')).toBe(false);
    expect(isTheme('')).toBe(false);
    expect(isTheme(null)).toBe(false);
    expect(isTheme(42)).toBe(false);
  });
});

describe('resolveTheme', () => {
  it('passes through a known theme', () => {
    expect(resolveTheme('nord')).toBe('nord');
  });

  it('maps a renamed theme onto its current name', () => {
    expect(resolveTheme('catppuccin')).toBe('catppuccin-mocha');
  });

  it('returns null for anything it does not recognize', () => {
    expect(resolveTheme('not-a-theme')).toBeNull();
    expect(resolveTheme(null)).toBeNull();
  });

  it('does not resolve inherited Object properties', () => {
    expect(resolveTheme('constructor')).toBeNull();
    expect(resolveTheme('toString')).toBeNull();
  });
});

describe('theme storage', () => {
  beforeEach(() => {
    window.localStorage.clear();
    resetThemeStore();
    document.documentElement.removeAttribute('data-theme');
  });

  afterEach(() => {
    vi.restoreAllMocks();
  });

  it('round-trips a theme through localStorage', () => {
    saveTheme('dracula');
    expect(window.localStorage.getItem(THEME_STORAGE_KEY)).toBe('dracula');
    expect(loadTheme()).toBe('dracula');
  });

  it('upgrades a stored legacy name on read', () => {
    window.localStorage.setItem(THEME_STORAGE_KEY, 'catppuccin');
    expect(loadTheme()).toBe('catppuccin-mocha');
  });

  it('falls back to the default when the stored value is unusable', () => {
    window.localStorage.setItem(THEME_STORAGE_KEY, 'deleted-theme');
    expect(loadTheme()).toBe(DEFAULT_THEME);
  });

  // jsdom does not implement matchMedia, which is also why systemTheme() calls
  // it optionally rather than assuming it exists.
  it('follows the OS preference when nothing is stored', () => {
    vi.stubGlobal(
      'matchMedia',
      vi.fn(() => ({ matches: true }) as unknown as MediaQueryList)
    );
    expect(loadTheme()).toBe('light');
    vi.unstubAllGlobals();
  });

  it('uses the default theme when the OS preference is unavailable', () => {
    expect(window.matchMedia).toBeUndefined();
    expect(loadTheme()).toBe(DEFAULT_THEME);
  });

  it('survives storage being unavailable', () => {
    vi.spyOn(Storage.prototype, 'getItem').mockImplementation(() => {
      throw new Error('SecurityError');
    });
    vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => {
      throw new Error('QuotaExceededError');
    });

    expect(loadTheme()).toBe(DEFAULT_THEME);
    expect(() => saveTheme('nord')).not.toThrow();
  });
});

describe('theme store', () => {
  beforeEach(() => {
    window.localStorage.clear();
    resetThemeStore();
    document.documentElement.removeAttribute('data-theme');
  });

  it('publishes the theme to the document and to storage', () => {
    setTheme('gruvbox-dark');
    expect(getTheme()).toBe('gruvbox-dark');
    expect(document.documentElement.getAttribute('data-theme')).toBe('gruvbox-dark');
    expect(window.localStorage.getItem(THEME_STORAGE_KEY)).toBe('gruvbox-dark');
  });

  it('notifies subscribers on change, and stops after unsubscribe', () => {
    const listener = vi.fn();
    const unsubscribe = subscribeToTheme(listener);

    setTheme('monokai');
    expect(listener).toHaveBeenCalledTimes(1);

    // Re-selecting the active theme is a no-op.
    setTheme('monokai');
    expect(listener).toHaveBeenCalledTimes(1);

    unsubscribe();
    setTheme('nord');
    expect(listener).toHaveBeenCalledTimes(1);
  });

  it('adopts a theme chosen in another tab', () => {
    const listener = vi.fn();
    const unsubscribe = subscribeToTheme(listener);
    setTheme('nord');
    listener.mockClear();

    window.localStorage.setItem(THEME_STORAGE_KEY, 'tokyo-night');
    window.dispatchEvent(new StorageEvent('storage', { key: THEME_STORAGE_KEY }));

    expect(getTheme()).toBe('tokyo-night');
    expect(document.documentElement.getAttribute('data-theme')).toBe('tokyo-night');
    expect(listener).toHaveBeenCalledTimes(1);

    unsubscribe();
  });

  it('ignores storage events for other keys', () => {
    const unsubscribe = subscribeToTheme(() => {});
    setTheme('nord');

    window.localStorage.setItem('prism.something-else', 'dracula');
    window.dispatchEvent(new StorageEvent('storage', { key: 'prism.something-else' }));

    expect(getTheme()).toBe('nord');
    unsubscribe();
  });
});

describe('theme catalog', () => {
  it('offers exactly one menu entry per theme', () => {
    expect(THEME_OPTIONS.map((option) => option.value).sort()).toEqual([...VALID_THEMES].sort());
  });

  it('gives every theme a non-empty label', () => {
    for (const option of THEME_OPTIONS) {
      expect(option.label.trim()).not.toBe('');
    }
  });

  it('has a palette for every theme, and no palette without a theme', () => {
    // Anchored to the line start so prose in the file's header comment, which
    // also names the selector, is not mistaken for a palette.
    const declared = [...palettesScss.matchAll(/^\[data-theme='([^']+)'\]/gm)].map((match) => match[1]);
    expect(declared.sort()).toEqual([...VALID_THEMES].sort());
  });

  it('gives every palette the same set of tokens as the default theme', () => {
    const blocks = [...palettesScss.matchAll(/\{([^}]*)\}/g)].map((match) =>
      [...match[1].matchAll(/(--[\w-]+):/g)].map((token) => token[1]).sort()
    );
    const [defaultTokens, ...rest] = blocks;
    expect(defaultTokens.length).toBeGreaterThan(0);
    for (const tokens of rest) {
      expect(tokens).toEqual(defaultTokens);
    }
  });

  it('keeps the pre-paint script in index.html on the same storage key', () => {
    expect(indexHtml).toContain(THEME_STORAGE_KEY);
  });
});
