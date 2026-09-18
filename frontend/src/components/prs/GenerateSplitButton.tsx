import { useCallback } from 'react';
import { createPortal } from 'react-dom';
import { useDropdown } from '@/hooks/useDropdown';
import type { ReviewProfile } from '@/types/pr';
import { PILOT_BLOCKED_TITLE } from './publishPolicy';
import { PROFILE_CHOICES } from './reviewProfiles';
import './GenerateSplitButton.scss';

// Keep in sync with $panel-width in GenerateSplitButton.scss.
const PANEL_WIDTH = 260;

interface GenerateSplitButtonProps {
  /** Starts a review; publish=true also posts it to the GitHub PR. An explicit
   *  profile overrides the deployment default. */
  onGenerate: (publish: boolean, profile?: ReviewProfile) => void;
  /** True while the trigger-review mutation is in flight. */
  pending: boolean;
  /** False when the PR author is outside the publish pilot, so posting is not offered. */
  publishAllowed: boolean;
}

/**
 * Review-cell control for PRs with no current review. The primary segment
 * runs the default (post to PR when allowed, otherwise dashboard only); the
 * chevron opens a menu that makes either choice explicit.
 */
export function GenerateSplitButton({ onGenerate, pending, publishAllowed }: GenerateSplitButtonProps) {
  const { isOpen, toggle, close, anchorRef, panelRef, position } = useDropdown({
    panelWidth: PANEL_WIDTH,
    align: 'right',
    closeOnOutsideClick: true,
    closeOnEscape: true,
  });

  const handlePrimary = useCallback(() => onGenerate(publishAllowed), [onGenerate, publishAllowed]);

  const choose = useCallback((publish: boolean, profile?: ReviewProfile) => {
    if (profile) onGenerate(publish, profile);
    else onGenerate(publish);
    close();
  }, [onGenerate, close]);

  const primaryTitle = publishAllowed
    ? 'Generate an AI review and post it to the PR'
    : 'Generate an AI review (review HTML only; author is not in the comment pilot)';

  return (
    <span
      ref={anchorRef as React.RefObject<HTMLSpanElement>}
      className={`generate-split${pending ? ' generate-split--pending' : ''}`}
    >
      <button
        type="button"
        className="generate-split__primary"
        onClick={handlePrimary}
        disabled={pending}
        title={primaryTitle}
      >
        {pending ? 'Starting…' : '🔄 Generate'}
      </button>
      <button
        type="button"
        className="generate-split__chevron"
        aria-label="More generate options"
        aria-haspopup="menu"
        aria-expanded={isOpen}
        onClick={toggle}
        disabled={pending}
      >
        ▾
      </button>

      {isOpen && createPortal(
        <div
          ref={panelRef}
          className="generate-split__menu"
          role="menu"
          style={{ top: position.top, left: position.left, width: PANEL_WIDTH }}
        >
          <button
            type="button"
            role="menuitem"
            className="generate-split__item"
            onClick={() => choose(true)}
            disabled={!publishAllowed}
            title={publishAllowed ? undefined : PILOT_BLOCKED_TITLE}
          >
            <span className="generate-split__item-title">Generate and post PR comment</span>
            <span className="generate-split__item-desc">Summary and inline comments posted as the Prism bot</span>
          </button>
          <button
            type="button"
            role="menuitem"
            className="generate-split__item"
            onClick={() => choose(false)}
          >
            <span className="generate-split__item-title">Generate review HTML only</span>
            <span className="generate-split__item-desc">Dashboard report only, nothing posted to GitHub</span>
          </button>
          <div className="generate-split__divider" role="separator" />
          {PROFILE_CHOICES.map((choice) => (
            <button
              key={choice.profile}
              type="button"
              role="menuitem"
              className="generate-split__item"
              onClick={() => choose(publishAllowed, choice.profile)}
            >
              <span className="generate-split__item-title">{choice.title}</span>
              <span className="generate-split__item-desc">{choice.description}</span>
            </button>
          ))}
        </div>,
        document.body
      )}
    </span>
  );
}
