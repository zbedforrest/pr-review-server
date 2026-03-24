import { useCallback, useRef, useEffect } from 'react';
import { trackEvents, type TelemetryEventPayload } from '@/api/telemetry';

const FLUSH_INTERVAL_MS = 5000;
const FLUSH_THRESHOLD = 20;
const SEARCH_DEBOUNCE_MS = 1000;

// Singleton queue shared across all hook instances
// eslint-disable-next-line prefer-const -- mutated via push/splice
let queue: TelemetryEventPayload[] = [];
let flushTimer: ReturnType<typeof setInterval> | null = null;
let instanceCount = 0;

function flush() {
  if (queue.length === 0) return;
  const batch = queue.splice(0);
  trackEvents(batch).catch((err) => {
    console.error('[telemetry] flush failed:', err);
  });
}

function enqueue(event: TelemetryEventPayload) {
  queue.push(event);
  if (queue.length >= FLUSH_THRESHOLD) {
    flush();
  }
}

function startFlushTimer() {
  if (flushTimer) return;
  flushTimer = setInterval(flush, FLUSH_INTERVAL_MS);
}

function stopFlushTimer() {
  if (flushTimer) {
    clearInterval(flushTimer);
    flushTimer = null;
  }
}

export function useTelemetry() {
  const searchTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null);

  // Manage singleton lifecycle
  useEffect(() => {
    instanceCount++;
    startFlushTimer();
    return () => {
      instanceCount--;
      if (instanceCount === 0) {
        flush();
        stopFlushTimer();
      }
    };
  }, []);

  const track = useCallback(
    (action: string, opts?: { label?: string; pr_owner?: string; pr_repo?: string; pr_number?: number }) => {
      enqueue({
        action,
        label: opts?.label,
        pr_owner: opts?.pr_owner,
        pr_repo: opts?.pr_repo,
        pr_number: opts?.pr_number,
      });
    },
    []
  );

  const trackSearch = useCallback((term: string) => {
    if (searchTimerRef.current) {
      clearTimeout(searchTimerRef.current);
    }
    if (!term.trim()) return;
    searchTimerRef.current = setTimeout(() => {
      enqueue({ action: 'search', label: term.trim() });
    }, SEARCH_DEBOUNCE_MS);
  }, []);

  return { track, trackSearch };
}
