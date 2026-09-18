import { Fragment, forwardRef, useCallback, useEffect, useId, useRef, useState, type KeyboardEvent, type MouseEvent } from 'react';
import type { QuickAction } from '@/api/prActions';
import type { PR } from '@/types/pr';
import { QUICK_ACTIONS_LEAF_WIDTH, focusableMenuItems, roveMenuFocus } from './menuKeyboard';
import type { QuickActionAvailabilityMap } from './quickActionPolicy';
import './QuickActionsSubmenu.scss';

const COPY_RESET_MS = 1500;

interface QuickActionsSubmenuProps {
  pr: PR;
  availability: QuickActionAvailabilityMap;
  /** Signed-in GitHub login the actions are attributed to. */
  login: string | undefined;
  /** No usable GitHub token: show the sign-in item. */
  needsSignIn: boolean;
  onChoose: (action: QuickAction) => void;
  onOpenGitHub: (e: MouseEvent<HTMLAnchorElement>) => void;
  onCopyLink: () => void;
  /** Close the leaf only; called for Escape and ArrowLeft. */
  onClose: () => void;
  /** Render as an accordion under the item instead of a flyout. */
  inline: boolean;
  style?: React.CSSProperties;
  onMouseEnter?: () => void;
  onMouseLeave?: () => void;
}

const REVIEW_ITEMS: { action: QuickAction; icon: string; label: string; className?: string }[] = [
  { action: 'approve', icon: '✓', label: 'Approve' },
  { action: 'request_changes', icon: '✗', label: 'Request changes…', className: 'quick-actions__item--danger' },
  { action: 'comment', icon: '💬', label: 'Comment…' },
];

export const QuickActionsSubmenu = forwardRef<HTMLDivElement, QuickActionsSubmenuProps>(function QuickActionsSubmenu(
  { pr, availability, login, needsSignIn, onChoose, onOpenGitHub, onCopyLink, onClose, inline, style, onMouseEnter, onMouseLeave },
  ref
) {
  const describeId = useId();
  const [copyStatus, setCopyStatus] = useState<'idle' | 'copied' | 'error'>('idle');
  const copyResetTimer = useRef<ReturnType<typeof setTimeout> | null>(null);
  const prUrl = `https://github.com/${pr.owner}/${pr.repo}/pull/${pr.number}`;

  useEffect(() => () => {
    if (copyResetTimer.current) clearTimeout(copyResetTimer.current);
  }, []);

  const handleCopy = useCallback(async () => {
    onCopyLink();
    try {
      await navigator.clipboard.writeText(prUrl);
      setCopyStatus('copied');
    } catch {
      setCopyStatus('error');
    }
    if (copyResetTimer.current) clearTimeout(copyResetTimer.current);
    copyResetTimer.current = setTimeout(() => setCopyStatus('idle'), COPY_RESET_MS);
  }, [onCopyLink, prUrl]);

  const handleKeyDown = useCallback(
    (e: KeyboardEvent<HTMLDivElement>) => {
      if (e.key === 'Escape' || e.key === 'ArrowLeft') {
        e.preventDefault();
        e.stopPropagation();
        onClose();
        return;
      }
      const root = e.currentTarget;
      if (roveMenuFocus(focusableMenuItems(root), e.key, document.activeElement)) {
        e.preventDefault();
        e.stopPropagation();
      }
    },
    [onClose]
  );

  const copyLabel = copyStatus === 'copied' ? 'Copied!' : copyStatus === 'error' ? 'Copy failed' : 'Copy link';

  return (
    <div
      ref={ref}
      className={`quick-actions${inline ? ' quick-actions--inline' : ''}`}
      role="menu"
      aria-label="Quick actions"
      style={inline ? undefined : { ...style, width: QUICK_ACTIONS_LEAF_WIDTH }}
      onKeyDown={handleKeyDown}
      onMouseEnter={onMouseEnter}
      onMouseLeave={onMouseLeave}
    >
      {REVIEW_ITEMS.map(({ action, icon, label, className }) => {
        const state = availability[action];
        const reasonId = `${describeId}-${action}`;
        return (
          <Fragment key={action}>
            <button
              type="button"
              role="menuitem"
              className={`quick-actions__item${className ? ` ${className}` : ''}`}
              disabled={!state.enabled}
              aria-disabled={!state.enabled}
              title={state.reason}
              aria-describedby={state.reason ? reasonId : undefined}
              onClick={() => onChoose(action)}
            >
              <span className="quick-actions__icon" aria-hidden="true">{icon}</span> {label}
            </button>
            {state.reason && <span id={reasonId} className="quick-actions__sr-only">{state.reason}</span>}
          </Fragment>
        );
      })}

      <div className="quick-actions__divider" role="separator" />

      <a
        className="quick-actions__item"
        role="menuitem"
        href={prUrl}
        target="_blank"
        rel="noopener noreferrer"
        onClick={onOpenGitHub}
      >
        <span className="quick-actions__icon" aria-hidden="true">↗</span> Open on GitHub
      </a>
      <button
        type="button"
        role="menuitem"
        className="quick-actions__item"
        disabled={!availability.copy.enabled}
        aria-disabled={!availability.copy.enabled}
        title={availability.copy.reason}
        onClick={handleCopy}
      >
        <span className="quick-actions__icon" aria-hidden="true">⧉</span> {copyLabel}
      </button>
      {needsSignIn && (
        <a className="quick-actions__item quick-actions__item--signin" role="menuitem" href="/login">
          <span className="quick-actions__icon" aria-hidden="true">🔑</span> Sign in again to enable…
        </a>
      )}

      <div className="quick-actions__footer">
        {needsSignIn ? 'GitHub actions need a fresh sign-in' : login ? `acts as @${login} on GitHub` : 'acts as you on GitHub'}
      </div>
    </div>
  );
});
