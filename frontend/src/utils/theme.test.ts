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
  LEGACY_THEME_ALIASES,
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

  it('upgrades a stored legacy name on read and persists the current name', () => {
    window.localStorage.setItem(THEME_STORAGE_KEY, 'catppuccin');
    expect(loadTheme()).toBe('catppuccin-mocha');
    expect(window.localStorage.getItem(THEME_STORAGE_KEY)).toBe('catppuccin-mocha');
  });

  it('does not rewrite storage when the stored name is already current', () => {
    window.localStorage.setItem(THEME_STORAGE_KEY, 'nord');
    const setItem = vi.spyOn(Storage.prototype, 'setItem');
    expect(loadTheme()).toBe('nord');
    expect(setItem).not.toHaveBeenCalled();
  });

  it('leaves an unusable stored value alone', () => {
    window.localStorage.setItem(THEME_STORAGE_KEY, 'deleted-theme');
    loadTheme();
    expect(window.localStorage.getItem(THEME_STORAGE_KEY)).toBe('deleted-theme');
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

  it('declares a color-scheme on every palette that matches its background', () => {
    const blocks = [...palettesScss.matchAll(/\{([^}]*)\}/g)].map((match) => match[1]);
    const schemeFor = (hex: string): 'light' | 'dark' => {
      const [r, g, b] = [1, 3, 5].map((i) => parseInt(hex.slice(i, i + 2), 16) / 255);
      return 0.2126 * r + 0.7152 * g + 0.0722 * b > 0.5 ? 'light' : 'dark';
    };
    expect(blocks.length).toBe(VALID_THEMES.length);
    for (const block of blocks) {
      const background = /--bg-primary:\s*(#[0-9a-f]{6})/i.exec(block)?.[1];
      expect(background).toBeDefined();
      expect(/color-scheme:\s*(\w+)/.exec(block)?.[1]).toBe(schemeFor(background!));
    }
  });

  it('keeps the pre-paint script in index.html on the same storage key', () => {
    expect(indexHtml).toContain(THEME_STORAGE_KEY);
  });
});

describe('pre-paint script', () => {
  const script = /<script>([\s\S]*?)<\/script>/.exec(indexHtml)?.[1] ?? '';

  function paintWith({ stored, storageBlocked = false, prefersLight = false }: {
    stored?: string;
    storageBlocked?: boolean;
    prefersLight?: boolean;
  }): string | null {
    if (storageBlocked) {
      vi.spyOn(Storage.prototype, 'getItem').mockImplementation(() => {
        throw new Error('SecurityError');
      });
    } else if (stored !== undefined) {
      window.localStorage.setItem(THEME_STORAGE_KEY, stored);
    }
    vi.stubGlobal(
      'matchMedia',
      vi.fn(() => ({ matches: prefersLight }) as unknown as MediaQueryList)
    );
    new Function(script)();
    return document.documentElement.getAttribute('data-theme');
  }

  beforeEach(() => {
    window.localStorage.clear();
    document.documentElement.removeAttribute('data-theme');
  });

  afterEach(() => {
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
  });

  it('is non-empty', () => {
    expect(script).toContain('data-theme');
  });

  it('applies the stored theme', () => {
    expect(paintWith({ stored: 'nord', prefersLight: true })).toBe('nord');
  });

  it('follows the OS preference when nothing is stored', () => {
    expect(paintWith({ prefersLight: true })).toBe('light');
    expect(paintWith({ prefersLight: false })).toBe('dark');
  });

  it('still follows the OS preference when storage is blocked', () => {
    expect(paintWith({ storageBlocked: true, prefersLight: true })).toBe('light');
  });

  it('resolves a stored legacy alias to its current name', () => {
    expect(paintWith({ stored: 'catppuccin', prefersLight: true })).toBe('catppuccin-mocha');
  });

  it('falls back to the OS preference for a value that names no theme', () => {
    expect(paintWith({ stored: 'deleted-theme', prefersLight: true })).toBe('light');
    expect(paintWith({ stored: 'constructor', prefersLight: true })).toBe('light');
  });

  it('knows exactly the theme names and aliases that theme.ts does', () => {
    const literal = (name: string): unknown => {
      const source = new RegExp(`var ${name} = ([^;]*);`).exec(script)?.[1];
      expect(source, `var ${name} in index.html`).toBeDefined();
      return new Function(`return ${source}`)();
    };
    expect([...(literal('themes') as string[])].sort()).toEqual([...VALID_THEMES].sort());
    expect(literal('aliases')).toEqual(LEGACY_THEME_ALIASES);
  });
});

describe('stylesheets', () => {
  const stylesheets = import.meta.glob('/src/**/*.scss', {
    query: '?raw',
    import: 'default',
    eager: true,
  }) as Record<string, string>;

  // The $color-* variables are var() references, which Sass color functions
  // cannot evaluate: they pass through and the browser drops the declaration.
  it('never applies a Sass color function to a theme color', () => {
    const offenders = Object.entries(stylesheets).flatMap(([file, source]) =>
      [...source.matchAll(/\b(?:rgba?|hsla?|darken|lighten|mix|transparentize|fade-out|opacify|color\.\w+)\(\$color-[^)]*\)/g)].map(
        (match) => `${file}: ${match[0]}`
      )
    );
    expect(Object.keys(stylesheets).length).toBeGreaterThan(0);
    expect(offenders).toEqual([]);
  });
});
