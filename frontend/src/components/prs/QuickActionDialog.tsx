import { useCallback, useEffect, useId, useLayoutEffect, useRef, useState, type KeyboardEvent, type RefObject } from 'react';
import { createPortal } from 'react-dom';
import { APIError } from '@/api/client';
import type { QuickAction } from '@/api/prActions';
import type { PR } from '@/types/pr';
import { QUICK_ACTION_BODY_MAX, QUICK_ACTION_COUNTER_THRESHOLD, QUICK_ACTION_COPY, quickActionErrorMessage, shortSha } from './quickActionCopy';
import './QuickActionDialog.scss';

interface QuickActionDialogProps {
  pr: PR;
  action: QuickAction;
  login: string | undefined;
  onSubmit: (body: string, expectedHeadSha: string) => void;
  pending: boolean;
  error: Error | null;
  /** New head sha reported by a 409 head_moved; switches the dialog to confirm-and-resubmit. */
  headMovedTo?: string;
  onClose: () => void;
  /** Receives focus when the dialog closes (the row's kebab). */
  returnFocusRef?: RefObject<HTMLElement>;
}

const FOCUSABLE = 'button:not([disabled]), textarea:not([disabled]), a[href], [tabindex]:not([tabindex="-1"])';

export function QuickActionDialog({
  pr,
  action,
  login,
  onSubmit,
  pending,
  error,
  headMovedTo,
  onClose,
  returnFocusRef,
}: QuickActionDialogProps) {
  const copy = QUICK_ACTION_COPY[action];
  const titleId = useId();
  const rootRef = useRef<HTMLDivElement>(null);
  const textareaRef = useRef<HTMLTextAreaElement>(null);
  const primaryRef = useRef<HTMLButtonElement>(null);
  const [body, setBody] = useState('');
  // The head the user saw when the dialog opened; a row refresh must not
  // retarget the review, only an explicit head-moved confirmation may.
  const [openedSha] = useState(pr.commit_sha);
  // pending only flips after the mutation rerenders, so a double click or a
  // repeated Ctrl+Enter could post twice with fresh request ids.
  const submitLock = useRef(false);

  const movedTo = headMovedTo && headMovedTo !== openedSha ? headMovedTo : undefined;
  const expectedHeadSha = movedTo ?? openedSha;
  const canSubmit = !pending && (!copy.requiresBody || body.trim() !== '');
  const errorCode = error instanceof APIError ? error.code : undefined;
  const showError = error !== null && !(errorCode === 'head_moved' && movedTo);
  const prLabel = `${pr.owner}/${pr.repo} #${pr.number}`;
  const prUrl = `https://github.com/${pr.owner}/${pr.repo}/pull/${pr.number}`;

  useLayoutEffect(() => {
    const target = action === 'approve' ? primaryRef.current : textareaRef.current;
    target?.focus();
  }, [action]);

  useEffect(() => {
    const ref = returnFocusRef;
    return () => ref?.current?.focus();
  }, [returnFocusRef]);

  useEffect(() => {
    if (!pending) submitLock.current = false;
  });

  const submit = useCallback(() => {
    if (!canSubmit || submitLock.current) return;
    submitLock.current = true;
    onSubmit(body, expectedHeadSha);
  }, [canSubmit, onSubmit, body, expectedHeadSha]);

  const handleKeyDown = useCallback(
    (e: KeyboardEvent<HTMLDivElement>) => {
      if (e.key === 'Escape') {
        e.preventDefault();
        e.stopPropagation();
        if (!pending) onClose();
        return;
      }
      if (e.key === 'Enter' && (e.ctrlKey || e.metaKey)) {
        e.preventDefault();
        submit();
        return;
      }
      if (e.key === 'Tab') {
        const nodes = Array.from(rootRef.current?.querySelectorAll<HTMLElement>(FOCUSABLE) ?? []);
        if (nodes.length === 0) return;
        const first = nodes[0];
        const last = nodes[nodes.length - 1];
        if (e.shiftKey && document.activeElement === first) {
          e.preventDefault();
          last.focus();
        } else if (!e.shiftKey && document.activeElement === last) {
          e.preventDefault();
          first.focus();
        }
      }
    },
    [onClose, pending, submit]
  );

  const primaryLabel = pending ? copy.pending : movedTo ? `${copy.verb} ${shortSha(movedTo)}` : copy.verb;

  return createPortal(
    <div className="quick-action-dialog__overlay" onMouseDown={(e) => { if (e.target === e.currentTarget && !pending) onClose(); }}>
      <div
        ref={rootRef}
        className="quick-action-dialog"
        role="dialog"
        aria-modal="true"
        aria-labelledby={titleId}
        onKeyDown={handleKeyDown}
      >
        <div className="quick-action-dialog__header">
          <h2 id={titleId} className="quick-action-dialog__title">
            {copy.title} {prLabel}
          </h2>
          <button
            type="button"
            className="quick-action-dialog__close"
            aria-label="Close"
            onClick={onClose}
            disabled={pending}
          >
            ✕
          </button>
        </div>
        <div className="quick-action-dialog__subtitle">
          “{pr.title}” · <span className="quick-action-dialog__sha">{shortSha(openedSha)}</span>
        </div>

        <label className="quick-action-dialog__label" htmlFor={`${titleId}-body`}>
          Comment {copy.requiresBody ? '(required)' : '(optional)'}
        </label>
        <textarea
          id={`${titleId}-body`}
          ref={textareaRef}
          className="quick-action-dialog__textarea"
          rows={4}
          maxLength={QUICK_ACTION_BODY_MAX}
          value={body}
          onChange={(e) => setBody(e.target.value)}
          disabled={pending}
        />
        {body.length > QUICK_ACTION_COUNTER_THRESHOLD && (
          <div className="quick-action-dialog__counter">
            {body.length.toLocaleString()} / {QUICK_ACTION_BODY_MAX.toLocaleString()}
          </div>
        )}

        {movedTo ? (
          <div className="quick-action-dialog__notice" role="status">
            The PR head moved from {shortSha(openedSha)} to {shortSha(movedTo)} since this row loaded.{' '}
            {copy.verb} the new head?
          </div>
        ) : (
          <div className="quick-action-dialog__hint">
            Posts to GitHub as @{login ?? 'you'}. Reviewed head {shortSha(openedSha)}; if the head has moved you
            will be asked before anything is posted.
          </div>
        )}

        {showError && (
          <div className="error-message quick-action-dialog__error" role="alert">
            {quickActionErrorMessage(action, error)}{' '}
            {errorCode === 'reauth_required' ? (
              <a className="quick-action-dialog__error-link" href="/login">Sign in ↗</a>
            ) : (
              <a className="quick-action-dialog__error-link" href={prUrl} target="_blank" rel="noopener noreferrer">
                Open on GitHub ↗
              </a>
            )}
          </div>
        )}

        <div className="quick-action-dialog__actions">
          <button type="button" className="quick-action-dialog__button" onClick={onClose} disabled={pending}>
            Cancel
          </button>
          <button
            ref={primaryRef}
            type="button"
            className={`quick-action-dialog__button quick-action-dialog__button--primary quick-action-dialog__button--${copy.tone}`}
            onClick={submit}
            disabled={!canSubmit}
          >
            {primaryLabel}
          </button>
        </div>
      </div>
    </div>,
    document.body
  );
}
