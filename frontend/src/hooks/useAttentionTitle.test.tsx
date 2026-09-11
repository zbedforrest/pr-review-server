import { renderHook } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { useAttentionTitle } from './useAttentionTitle';

describe('useAttentionTitle', () => {
  beforeEach(() => {
    document.title = 'PRism';
  });

  afterEach(() => {
    document.title = '';
  });

  it('prefixes the base title with the count while it is positive', () => {
    renderHook(() => useAttentionTitle(2));

    expect(document.title).toBe('(2) PRism');
  });

  it('leaves the base title alone when the count is zero', () => {
    renderHook(() => useAttentionTitle(0));

    expect(document.title).toBe('PRism');
  });

  it('replaces the prefix instead of stacking when the count changes', () => {
    const { rerender } = renderHook((count: number) => useAttentionTitle(count), { initialProps: 2 });
    rerender(3);

    expect(document.title).toBe('(3) PRism');
  });

  it('restores the base title when the count returns to zero', () => {
    const { rerender } = renderHook((count: number) => useAttentionTitle(count), { initialProps: 2 });
    rerender(0);

    expect(document.title).toBe('PRism');
  });

  it('restores the base title on unmount', () => {
    const { unmount } = renderHook(() => useAttentionTitle(4));
    expect(document.title).toBe('(4) PRism');

    unmount();

    expect(document.title).toBe('PRism');
  });
});
