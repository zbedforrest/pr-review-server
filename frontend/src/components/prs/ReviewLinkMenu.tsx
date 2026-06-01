import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { createPortal } from 'react-dom';
import type { PR } from '@/types/pr';
import { useTelemetry } from '@/hooks/useTelemetry';
import { formatRelativeTime } from '@/utils/formatDate';
import './ReviewLinkMenu.scss';

interface ReviewLinkMenuProps {
  pr: PR;
  /** URL to the rendered AI review (only passed when a review exists). */
  reviewUrl: string;
}

// Keep in sync with $panel-width in ReviewLinkMenu.scss.
const PANEL_WIDTH = 240;
const OPEN_DELAY_MS = 250;
const CLOSE_DELAY_MS = 200;
const VIEWPORT_MARGIN = 8;

interface PanelPosition {
  top: number;
  left: number;
}

export function ReviewLinkMenu({ pr, reviewUrl }: ReviewLinkMenuProps) {
  const { track } = useTelemetry();
  const anchorRef = useRef<HTMLSpanElement>(null);
  const openTimer = useRef<ReturnType<typeof setTimeout> | null>(null);
  const closeTimer = useRef<ReturnType<typeof setTimeout> | null>(null);
  const copyResetTimer = useRef<ReturnType<typeof setTimeout> | null>(null);

  const [isOpen, setIsOpen] = useState(false);
  const [position, setPosition] = useState<PanelPosition>({ top: 0, left: 0 });
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

  const computePosition = useCallback(() => {
    const el = anchorRef.current;
    if (!el) return;
    const rect = el.getBoundingClientRect();
    // Right-align the panel to the link, clamped within the viewport.
    const maxLeft = window.innerWidth - PANEL_WIDTH - VIEWPORT_MARGIN;
    const left = Math.max(VIEWPORT_MARGIN, Math.min(rect.right - PANEL_WIDTH, maxLeft));
    setPosition({ top: rect.bottom + 6, left });
  }, []);

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
      computePosition();
      setIsOpen(true);
    }, OPEN_DELAY_MS);
  }, [isOpen, computePosition]);

  const scheduleClose = useCallback(() => {
    if (openTimer.current) {
      clearTimeout(openTimer.current);
      openTimer.current = null;
    }
    if (closeTimer.current) return;
    closeTimer.current = setTimeout(() => {
      closeTimer.current = null;
      setIsOpen(false);
    }, CLOSE_DELAY_MS);
  }, []);

  // Reposition while open if the page scrolls or resizes.
  useEffect(() => {
    if (!isOpen) return;
    const onReflow = () => computePosition();
    window.addEventListener('scroll', onReflow, true);
    window.addEventListener('resize', onReflow);
    return () => {
      window.removeEventListener('scroll', onReflow, true);
      window.removeEventListener('resize', onReflow);
    };
  }, [isOpen, computePosition]);

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

  const counts = [
    { key: 'critical', label: 'critical', value: pr.critical_count, cls: 'review-menu__dot--critical' },
    { key: 'medium', label: 'medium', value: pr.medium_count, cls: 'review-menu__dot--medium' },
    { key: 'low', label: 'low', value: pr.low_count, cls: 'review-menu__dot--low' },
  ];
  const hasFindings = counts.some(c => c.value > 0);
  const shortSha = pr.commit_sha ? pr.commit_sha.slice(0, 7) : '';

  return (
    <span
      ref={anchorRef}
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

      {isOpen && createPortal(
        <div
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
        </div>,
        document.body
      )}
    </span>
  );
}
