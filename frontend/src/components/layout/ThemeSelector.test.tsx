import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { ThemeSelector } from './ThemeSelector';
import { resetThemeStore, THEME_OPTIONS, THEME_STORAGE_KEY } from '@/utils/theme';

const track = vi.fn();
vi.mock('@/hooks/useTelemetry', () => ({
  useTelemetry: () => ({ track, trackSearch: vi.fn() }),
}));

describe('ThemeSelector', () => {
  beforeEach(() => {
    window.localStorage.clear();
    resetThemeStore();
    document.documentElement.removeAttribute('data-theme');
    track.mockClear();
  });

  afterEach(cleanup);

  it('lists every theme', () => {
    render(<ThemeSelector />);
    expect(screen.getAllByRole('option')).toHaveLength(THEME_OPTIONS.length);
  });

  it('shows the stored theme as selected', () => {
    window.localStorage.setItem(THEME_STORAGE_KEY, 'dracula');
    render(<ThemeSelector />);
    expect((screen.getByLabelText('Theme') as HTMLSelectElement).value).toBe('dracula');
  });

  it('applies, persists and reports the chosen theme', () => {
    render(<ThemeSelector />);

    fireEvent.change(screen.getByLabelText('Theme'), { target: { value: 'tokyo-night' } });

    expect(document.documentElement.getAttribute('data-theme')).toBe('tokyo-night');
    expect(window.localStorage.getItem(THEME_STORAGE_KEY)).toBe('tokyo-night');
    expect(track).toHaveBeenCalledWith('theme_change', { label: 'tokyo-night' });
  });
});
