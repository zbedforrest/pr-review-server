import { act, renderHook } from '@testing-library/react';
import { beforeEach, describe, expect, it } from 'vitest';
import { useTheme } from './useTheme';
import { resetThemeStore, THEME_STORAGE_KEY } from '@/utils/theme';

describe('useTheme', () => {
  beforeEach(() => {
    window.localStorage.clear();
    resetThemeStore();
    document.documentElement.removeAttribute('data-theme');
  });

  it('starts from the stored theme and applies it to the document', () => {
    window.localStorage.setItem(THEME_STORAGE_KEY, 'everforest');

    const { result } = renderHook(() => useTheme());

    expect(result.current.theme).toBe('everforest');
    expect(document.documentElement.getAttribute('data-theme')).toBe('everforest');
  });

  it('persists and applies a newly selected theme', () => {
    const { result } = renderHook(() => useTheme());

    act(() => result.current.setTheme('rose-pine'));

    expect(result.current.theme).toBe('rose-pine');
    expect(document.documentElement.getAttribute('data-theme')).toBe('rose-pine');
    expect(window.localStorage.getItem(THEME_STORAGE_KEY)).toBe('rose-pine');
  });

  it('keeps separate consumers in sync', () => {
    const first = renderHook(() => useTheme());
    const second = renderHook(() => useTheme());

    act(() => first.result.current.setTheme('solarized-light'));

    expect(second.result.current.theme).toBe('solarized-light');
  });

  it('re-applies the theme when the document attribute is lost', () => {
    const { result, rerender } = renderHook(() => useTheme());
    act(() => result.current.setTheme('nord'));

    document.documentElement.removeAttribute('data-theme');
    act(() => result.current.setTheme('monokai'));
    rerender();

    expect(document.documentElement.getAttribute('data-theme')).toBe('monokai');
  });
});
