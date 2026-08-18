import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { createPortal } from 'react-dom';
import type { PR } from '@/types/pr';
import { useTelemetry } from '@/hooks/useTelemetry';
import { useDropdown } from '@/hooks/useDropdown';
import { formatRelativeTime } from '@/utils/formatDate';
import { CriticalFindingsTriage } from './CriticalFindingsTriage';
import './ReviewLinkMenu.scss';

interface ReviewLinkMenuProps {
  pr: PR;
  /** URL to the rendered AI review (only passed when a review exists). */
  reviewUrl: string;
  /** Starts a fresh review for this PR (same flow as the row actions menu). */
  onTriggerReview: () => void;
  /** True while the trigger-review mutation is in flight. */
  reviewPending: boolean;
}

// Keep in sync with $panel-width in ReviewLinkMenu.scss.
const PANEL_WIDTH = 240;
const OPEN_DELAY_MS = 250;
const CLOSE_DELAY_MS = 200;

const compactModelName = (model: string) => model.split('/').pop() || model;

export function ReviewLinkMenu({ pr, reviewUrl, onTriggerReview, reviewPending }: ReviewLinkMenuProps) {
  const { track } = useTelemetry();
  // Positioning + reflow live in the shared hook; this menu drives open/close
  // off hover timers rather than clicks, so it wraps open()/close().
  const { isOpen, open, close, anchorRef, panelRef, position } = useDropdown({
    panelWidth: PANEL_WIDTH,
    align: 'right',
  });
  const openTimer = useRef<ReturnType<typeof setTimeout> | null>(null);
  const closeTimer = useRef<ReturnType<typeof setTimeout> | null>(null);
  const copyResetTimer = useRef<ReturnType<typeof setTimeout> | null>(null);

  const [copyStatus, setCopyStatus] = useState<'idle' | 'copied' | 'error'>('idle');

  const trackOpts = useMemo(
    () => ({ pr_owner: pr.owner, pr_repo: pr.repo, pr_number: pr.number }),
    [pr.owner, pr.repo, pr.number]
  );

  // Endpoint for the compact, coding-agent-optimized Markdown export. Used both
  // for the download link and for copying the contents to the clipboard.
  const exportUrl = useMemo(
    () => `/api/review/${pr.owner}/${pr.repo}/${pr.number}?format=md`,
    [pr.owner, pr.repo, pr.number]
  );

  const clearTimers = useCallback(() => {
    if (openTimer.current) clearTimeout(openTimer.current);
    if (closeTimer.current) clearTimeout(closeTimer.current);
    openTimer.current = null;
    closeTimer.current = null;
  }, []);

  const scheduleOpen = useCallback(() => {
    if (closeTimer.current) {
      clearTimeout(closeTimer.current);
      closeTimer.current = null;
    }
    if (isOpen || openTimer.current) return;
    openTimer.current = setTimeout(() => {
      openTimer.current = null;
      open();
    }, OPEN_DELAY_MS);
  }, [isOpen, open]);

  const scheduleClose = useCallback(() => {
    if (openTimer.current) {
      clearTimeout(openTimer.current);
      openTimer.current = null;
    }
    if (closeTimer.current) return;
    closeTimer.current = setTimeout(() => {
      closeTimer.current = null;
      close();
    }, CLOSE_DELAY_MS);
  }, [close]);

  // Clean up any pending timers on unmount.
  useEffect(() => () => {
    clearTimers();
    if (copyResetTimer.current) clearTimeout(copyResetTimer.current);
  }, [clearTimers]);

  // Fetch the compact Markdown export and copy it to the clipboard so the user
  // can paste the whole review into a coding agent. (Same content the "Export"
  // item downloads — copied as text rather than saved to a file.)
  const handleCopy = useCallback(async () => {
    track('copy_compressed_review', trackOpts);
    try {
      const res = await fetch(exportUrl, { credentials: 'same-origin' });
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      const text = await res.text();
      await navigator.clipboard.writeText(text);
      setCopyStatus('copied');
    } catch {
      setCopyStatus('error');
    }
    if (copyResetTimer.current) clearTimeout(copyResetTimer.current);
    copyResetTimer.current = setTimeout(() => setCopyStatus('idle'), 1500);
  }, [track, trackOpts, exportUrl]);

  // Once the trigger lands, the row flips to generating and this whole menu
  // unmounts; the pending/status guard covers the window in between.
  const reviewInFlight =
    reviewPending || pr.status === 'generating' || pr.status === 'agent_reviewing';

  const handleRegenerate = useCallback(() => {
    onTriggerReview();
    close();
  }, [onTriggerReview, close]);

  const counts = [
    { key: 'critical', label: 'critical', value: pr.critical_count, cls: 'review-menu__dot--critical' },
    { key: 'medium', label: 'medium', value: pr.medium_count, cls: 'review-menu__dot--medium' },
    { key: 'low', label: 'low', value: pr.low_count, cls: 'review-menu__dot--low' },
  ];
  const hasFindings = counts.some(c => c.value > 0);
  const shortSha = pr.commit_sha ? pr.commit_sha.slice(0, 7) : '';
  // Red "R" badge only for an explicit request-changes verdict on an existing
  // review; any other verdict (or none) renders exactly the pre-badge UI.
  const showVerdictBadge = !!reviewUrl && pr.review_verdict === 'request_changes';
  // Orange "F" badge: the review was generated by an agent that fell back to
  // a different model than requested.
  const showFallbackBadge = !!reviewUrl && !!pr.model_fallback;
  const modelUses = pr.review_run?.models ?? [];

  return (
    <span
      ref={anchorRef as React.RefObject<HTMLSpanElement>}
      className="review-menu__anchor"
      onMouseEnter={scheduleOpen}
      onMouseLeave={scheduleClose}
    >
      <a
        href={reviewUrl}
        target="_blank"
        rel="noopener noreferrer"
        className="pr-table__review-link"
        onClick={() => track('view_review', trackOpts)}
      >
        View&nbsp;▾
      </a>
      {showVerdictBadge && (
        <span
          className="review-menu__verdict-badge"
          role="img"
          aria-label="Verdict: request changes"
          title="Verdict: request changes"
        >
          R
        </span>
      )}
      {showFallbackBadge && (
        <span
          className="review-menu__fallback-badge"
          role="img"
          aria-label="Review generated on a fallback model"
          title="Review generated on a fallback model — not the configured agent model"
        >
          F
        </span>
      )}

      {isOpen && createPortal(
        <div
          ref={panelRef}
          className="review-menu"
          role="menu"
          style={{ top: position.top, left: position.left, width: PANEL_WIDTH }}
          onMouseEnter={scheduleOpen}
          onMouseLeave={scheduleClose}
        >
          <div className="review-menu__summary">
            <div className="review-menu__counts">
              {hasFindings ? (
                counts.map(c => (
                  <span key={c.key} className="review-menu__count" title={`${c.value} ${c.label}`}>
                    <span className={`review-menu__dot ${c.cls}`} />
                    {c.value}
                  </span>
                ))
              ) : (
                <span className="review-menu__count review-menu__count--clean">No findings</span>
              )}
            </div>
            <div className="review-menu__meta">
              Reviewed {formatRelativeTime(pr.last_reviewed_at)}
              {shortSha && <> · <span className="review-menu__sha">{shortSha}</span></>}
            </div>
            {modelUses.length > 0 && (
              <div className="review-menu__models" aria-label="Models used">
                <span className="review-menu__models-label">Models</span>
                {modelUses.map((model, index) => (
                  <span
                    key={`${model.stage}-${model.requested_model}`}
                    className="review-menu__model"
                    title={`${model.stage}: requested ${model.requested_model}${
                      model.serving_model_verified
                        ? `; served ${model.served_model || model.requested_model}`
                        : '; serving model not reported'
                    }`}
                  >
                    {index > 0 && <span className="review-menu__model-arrow">→</span>}
                    {compactModelName(model.served_model || model.requested_model)}
                  </span>
                ))}
              </div>
            )}
            {pr.review_run?.run_id && (
              pr.review_run.html_path ? (
                <a
                  className="review-menu__run-id"
                  href={`/reviews/${pr.review_run.html_path}`}
                  target="_blank"
                  rel="noopener noreferrer"
                  title={`Open immutable run ${pr.review_run.run_id}`}
                >
                  Run {pr.review_run.run_id.slice(4, 12)} ↗
                </a>
              ) : (
                <div className="review-menu__run-id" title={pr.review_run.run_id}>
                  Run {pr.review_run.run_id.slice(4, 12)}
                </div>
              )
            )}
          </div>

          <div className="review-menu__divider" />

          <a
            className="review-menu__item"
            href={reviewUrl}
            target="_blank"
            rel="noopener noreferrer"
            role="menuitem"
            onClick={() => track('view_review', trackOpts)}
          >
            <span className="review-menu__icon">↗</span> Open review
          </a>
          <a
            className="review-menu__item"
            href={exportUrl}
            download
            role="menuitem"
            onClick={() => track('export_compressed_review', trackOpts)}
          >
            <span className="review-menu__icon">⬇</span> Export compressed review
          </a>
          <button
            type="button"
            className="review-menu__item review-menu__item--button"
            role="menuitem"
            onClick={handleCopy}
          >
            <span className="review-menu__icon">⧉</span>{' '}
            {copyStatus === 'copied' ? 'Copied!' : copyStatus === 'error' ? 'Copy failed' : 'Copy compressed review'}
          </button>
          <button
            type="button"
            className="review-menu__item review-menu__item--button"
            role="menuitem"
            onClick={handleRegenerate}
            disabled={reviewInFlight}
          >
            <span className="review-menu__icon">🔄</span>{' '}
            {reviewInFlight ? 'Reviewing…' : 'Regenerate review'}
          </button>

          {/* CRITICAL-only outcome triage (dismiss-with-reason / acknowledge).
              Renders nothing unless the backend feature flag is on — the
              component probes the endpoint and hides itself on 404. */}
          {pr.critical_count > 0 && <CriticalFindingsTriage pr={pr} />}
        </div>,
        document.body
      )}
    </span>
  );
}
