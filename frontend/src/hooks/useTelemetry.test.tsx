import { act, cleanup, renderHook } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { useTelemetry } from './useTelemetry';

const trackEventsMock = vi.fn();
vi.mock('@/api/telemetry', () => ({
  trackEvents: (events: unknown) => trackEventsMock(events),
}));

describe('useTelemetry track', () => {
  beforeEach(() => {
    trackEventsMock.mockReset();
    trackEventsMock.mockResolvedValue(undefined);
    vi.useFakeTimers();
  });
  afterEach(() => {
    cleanup();
    vi.useRealTimers();
  });

  const flushed = () => trackEventsMock.mock.calls.flatMap((c) => c[0] as unknown[]);

  it('encodes the publish choice in the label so the wire payload stays unchanged', () => {
    const { result, unmount } = renderHook(() => useTelemetry());
    act(() => {
      result.current.track('trigger_review', { pr_owner: 'acme', pr_repo: 'example', pr_number: 7, publish: true });
      result.current.track('trigger_review', { pr_owner: 'acme', pr_repo: 'example', pr_number: 8, publish: false });
    });
    unmount();
    expect(flushed()).toEqual([
      { action: 'trigger_review', label: 'publish', pr_owner: 'acme', pr_repo: 'example', pr_number: 7 },
      { action: 'trigger_review', label: 'dashboard_only', pr_owner: 'acme', pr_repo: 'example', pr_number: 8 },
    ]);
  });

  it('leaves the label alone when publish is not given', () => {
    const { result, unmount } = renderHook(() => useTelemetry());
    act(() => {
      result.current.track('toggle_auto_review', { label: 'on' });
      result.current.track('view_review', { pr_owner: 'acme', pr_repo: 'example', pr_number: 7 });
    });
    unmount();
    expect(flushed()).toEqual([
      { action: 'toggle_auto_review', label: 'on', pr_owner: undefined, pr_repo: undefined, pr_number: undefined },
      { action: 'view_review', label: undefined, pr_owner: 'acme', pr_repo: 'example', pr_number: 7 },
    ]);
  });
});
