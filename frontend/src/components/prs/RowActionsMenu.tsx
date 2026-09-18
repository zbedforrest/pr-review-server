import { useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState, type KeyboardEvent, type MouseEvent } from 'react';
import { createPortal } from 'react-dom';
import type { APIError } from '@/api/client';
import type { QuickAction } from '@/api/prActions';
import type { CurrentUser } from '@/api/user';
import type { PR } from '@/types/pr';
import { useTelemetry } from '@/hooks/useTelemetry';
import { useDropdown } from '@/hooks/useDropdown';
import { PILOT_BLOCKED_TITLE } from './publishPolicy';
import { QuickActionDialog } from './QuickActionDialog';
import { QUICK_ACTIONS_LEAF_WIDTH, focusableMenuItems, roveMenuFocus } from './menuKeyboard';
import { QuickActionsSubmenu } from './QuickActionsSubmenu';
import { quickActionAvailability } from './quickActionPolicy';
import './RowActionsMenu.scss';

// Keep in sync with $panel-width in RowActionsMenu.scss.
const PANEL_WIDTH = 220;
export const LEAF_OPEN_DELAY_MS = 150;
export const LEAF_CLOSE_DELAY_MS = 200;
// Below this viewport width the leaf renders inline under its item.
export const LEAF_INLINE_BREAKPOINT = 480;

export interface QuickActionsWiring {
  user: CurrentUser | undefined;
  /** Resolves when GitHub accepted the review; rejects with the APIError otherwise. */
  onSubmit: (action: QuickAction, body: string, expectedHeadSha: string) => Promise<unknown>;
  onOpenGitHub: (e: MouseEvent<HTMLAnchorElement>) => void;
  onCopyLink: () => void;
  /** Called whenever the dialog closes so the owner can reset mutation state. */
  onDialogClose: () => void;
  pending: boolean;
  error: APIError | null;
}

interface RowActionsMenuProps {
  pr: PR;
  /** Starts a review; publish=true also posts it to the GitHub PR. */
  onTriggerReview: (publish: boolean) => void;
  onToggleHidden: () => void;
  onDelete: () => void;
  /** True while the trigger-review mutation is in flight. */
  reviewPending: boolean;
  /** True while the set-hidden mutation is in flight. */
  hiddenPending: boolean;
  /** True while the delete mutation is in flight. */
  deletePending: boolean;
  /** False when the PR author is outside the publish pilot, so posting is not offered. */
  publishAllowed: boolean;
  /** Omitted, or a user without the flag, renders the menu without the Quick actions item. */
  quickActions?: QuickActionsWiring;
}

type LeafOpenVia = 'hover' | 'click' | 'keyboard';

/**
 * Kebab menu for the Actions column. Collapses the per-row review, hide, and
 * delete controls into a single click-to-open dropdown.
 */
export function RowActionsMenu({
  pr,
  onTriggerReview,
  onToggleHidden,
  onDelete,
  reviewPending,
  hiddenPending,
  deletePending,
  publishAllowed,
  quickActions,
}: RowActionsMenuProps) {
  const { track } = useTelemetry();
  const { isOpen, toggle, close, anchorRef, panelRef, position } = useDropdown({
    panelWidth: PANEL_WIDTH,
    align: 'right',
    closeOnOutsideClick: true,
    closeOnEscape: true,
  });
  const {
    isOpen: leafIsOpen,
    open: leafOpen,
    close: leafClose,
    anchorRef: leafAnchorRef,
    panelRef: leafPanelRef,
    position: leafPosition,
  } = useDropdown({ panelWidth: QUICK_ACTIONS_LEAF_WIDTH, placement: 'right', gap: 4 });
  const leafOpenTimer = useRef<ReturnType<typeof setTimeout> | null>(null);
  const leafCloseTimer = useRef<ReturnType<typeof setTimeout> | null>(null);
  const focusLeafOnOpen = useRef(false);
  const focusFirstOnOpen = useRef(false);
  const wasOpen = useRef(false);
  const [dialogAction, setDialogAction] = useState<QuickAction | null>(null);

  const trackOpts = useMemo(
    () => ({ pr_owner: pr.owner, pr_repo: pr.repo, pr_number: pr.number }),
    [pr.owner, pr.repo, pr.number]
  );
  const user = quickActions?.user;
  const quickActionsEnabled = user?.quick_actions_enabled === true;
  const availability = useMemo(() => quickActionAvailability(pr, user), [pr, user]);
  const needsSignIn = user?.github_actions_available !== true;
  const leafInline = typeof window !== 'undefined' && window.innerWidth < LEAF_INLINE_BREAKPOINT;

  const reviewInFlight =
    reviewPending || pr.status === 'generating' || pr.status === 'agent_reviewing';
  const hasReview = !!pr.review_url;

  const clearLeafTimers = useCallback(() => {
    if (leafOpenTimer.current) clearTimeout(leafOpenTimer.current);
    if (leafCloseTimer.current) clearTimeout(leafCloseTimer.current);
    leafOpenTimer.current = null;
    leafCloseTimer.current = null;
  }, []);

  const openLeaf = useCallback((via: LeafOpenVia) => {
    clearLeafTimers();
    if (leafIsOpen) return;
    track('quick_actions_open', { ...trackOpts, label: via });
    focusLeafOnOpen.current = via === 'keyboard';
    leafOpen();
  }, [clearLeafTimers, leafIsOpen, leafOpen, track, trackOpts]);

  const closeLeaf = useCallback(() => {
    clearLeafTimers();
    leafClose();
  }, [clearLeafTimers, leafClose]);

  const scheduleLeafOpen = useCallback(() => {
    if (leafCloseTimer.current) {
      clearTimeout(leafCloseTimer.current);
      leafCloseTimer.current = null;
    }
    if (leafIsOpen || leafOpenTimer.current) return;
    leafOpenTimer.current = setTimeout(() => {
      leafOpenTimer.current = null;
      openLeaf('hover');
    }, LEAF_OPEN_DELAY_MS);
  }, [leafIsOpen, openLeaf]);

  const scheduleLeafClose = useCallback(() => {
    if (leafOpenTimer.current) {
      clearTimeout(leafOpenTimer.current);
      leafOpenTimer.current = null;
    }
    if (leafCloseTimer.current) return;
    leafCloseTimer.current = setTimeout(() => {
      leafCloseTimer.current = null;
      leafClose();
    }, LEAF_CLOSE_DELAY_MS);
  }, [leafClose]);

  const cancelLeafClose = useCallback(() => {
    if (leafCloseTimer.current) {
      clearTimeout(leafCloseTimer.current);
      leafCloseTimer.current = null;
    }
  }, []);

  useEffect(() => () => clearLeafTimers(), [clearLeafTimers]);

  useEffect(() => {
    if (!isOpen) closeLeaf();
  }, [isOpen, closeLeaf]);

  // Keyboard-opened menus land focus on the first enabled item; a closed menu
  // hands focus back to the kebab when it fell to body (Escape, item chosen).
  useLayoutEffect(() => {
    if (isOpen && focusFirstOnOpen.current) {
      focusFirstOnOpen.current = false;
      focusableMenuItems(panelRef.current).filter((el) => !leafPanelRef.current?.contains(el))[0]?.focus();
    } else if (!isOpen && wasOpen.current && document.activeElement === document.body) {
      anchorRef.current?.focus();
    }
    wasOpen.current = isOpen;
  }, [isOpen, anchorRef, panelRef, leafPanelRef]);

  useLayoutEffect(() => {
    if (leafIsOpen && focusLeafOnOpen.current) {
      focusLeafOnOpen.current = false;
      focusableMenuItems(leafPanelRef.current)[0]?.focus();
    }
  }, [leafIsOpen, leafPanelRef]);

  const handleToggle = useCallback(() => {
    if (!isOpen) {
      track('open_row_actions', trackOpts);
    }
    toggle();
  }, [isOpen, toggle, track, trackOpts]);

  const handleTriggerKeyDown = useCallback((e: KeyboardEvent<HTMLButtonElement>) => {
    if (e.key === 'Enter' || e.key === ' ') focusFirstOnOpen.current = !isOpen;
  }, [isOpen]);

  const handlePanelKeyDown = useCallback((e: KeyboardEvent<HTMLDivElement>) => {
    if (leafPanelRef.current?.contains(e.target as Node)) return;
    const items = focusableMenuItems(panelRef.current).filter((el) => !leafPanelRef.current?.contains(el));
    if (roveMenuFocus(items, e.key, document.activeElement)) e.preventDefault();
  }, [panelRef, leafPanelRef]);

  const handleReview = useCallback((publish: boolean) => {
    onTriggerReview(publish);
    close();
  }, [onTriggerReview, close]);

  const handleToggleHidden = useCallback(() => {
    onToggleHidden();
    close();
  }, [onToggleHidden, close]);

  const handleDelete = useCallback(() => {
    onDelete();
    close();
  }, [onDelete, close]);

  // Enter and Space arrive as a native click with detail 0; handling them on
  // keydown too would let Firefox's keyup click toggle the leaf shut again.
  const handleLeafItemClick = useCallback((e: MouseEvent<HTMLButtonElement>) => {
    if (leafIsOpen) closeLeaf();
    else openLeaf(e.detail === 0 ? 'keyboard' : 'click');
  }, [leafIsOpen, closeLeaf, openLeaf]);

  const handleLeafItemKeyDown = useCallback((e: KeyboardEvent<HTMLButtonElement>) => {
    if (e.key === 'ArrowRight') {
      e.preventDefault();
      e.stopPropagation();
      openLeaf('keyboard');
    }
  }, [openLeaf]);

  const handleLeafClose = useCallback(() => {
    closeLeaf();
    leafAnchorRef.current?.focus();
  }, [closeLeaf, leafAnchorRef]);

  const handleChoose = useCallback((action: QuickAction) => {
    closeLeaf();
    close();
    setDialogAction(action);
  }, [closeLeaf, close]);

  const closeDialog = useCallback(() => {
    setDialogAction(null);
    quickActions?.onDialogClose();
  }, [quickActions]);

  const handleDialogSubmit = useCallback((body: string, expectedHeadSha: string) => {
    if (!dialogAction || !quickActions) return;
    quickActions.onSubmit(dialogAction, body, expectedHeadSha).then(closeDialog, () => {});
  }, [dialogAction, quickActions, closeDialog]);

  const verb = hasReview ? 'Regenerate' : 'Generate';
  const headMovedTo = quickActions?.error?.code === 'head_moved' ? quickActions.error.details?.head_sha : undefined;

  return (
    <>
      <button
        ref={anchorRef as React.RefObject<HTMLButtonElement>}
        type="button"
        className="row-actions__trigger"
        aria-label="Actions"
        aria-haspopup="menu"
        aria-expanded={isOpen}
        title="Row actions"
        onClick={handleToggle}
        onKeyDown={handleTriggerKeyDown}
      >
        ⋮
      </button>

      {isOpen && createPortal(
        <div
          ref={panelRef}
          className="row-actions__menu"
          role="menu"
          style={{ top: position.top, left: position.left, width: PANEL_WIDTH }}
          onKeyDown={handlePanelKeyDown}
        >
          <button
            type="button"
            role="menuitem"
            className="row-actions__item"
            onClick={() => handleReview(true)}
            disabled={reviewInFlight || !publishAllowed}
            title={publishAllowed ? undefined : PILOT_BLOCKED_TITLE}
          >
            🔄 {verb} and post PR comment
          </button>
          <button
            type="button"
            role="menuitem"
            className="row-actions__item"
            onClick={() => handleReview(false)}
            disabled={reviewInFlight}
          >
            🔄 {verb} review HTML only
          </button>
          {quickActionsEnabled && quickActions && (
            <>
              <button
                ref={leafAnchorRef as React.RefObject<HTMLButtonElement>}
                type="button"
                role="menuitem"
                className="row-actions__item row-actions__item--submenu"
                aria-haspopup="menu"
                aria-expanded={leafIsOpen}
                onClick={handleLeafItemClick}
                onKeyDown={handleLeafItemKeyDown}
                onMouseEnter={scheduleLeafOpen}
                onMouseLeave={scheduleLeafClose}
              >
                ⚡ Quick actions <span className="row-actions__chevron" aria-hidden="true">▸</span>
              </button>
              {leafIsOpen && (
                <QuickActionsSubmenu
                  ref={leafPanelRef}
                  pr={pr}
                  availability={availability}
                  login={user?.github_username}
                  needsSignIn={needsSignIn}
                  onChoose={handleChoose}
                  onOpenGitHub={quickActions.onOpenGitHub}
                  onCopyLink={quickActions.onCopyLink}
                  onClose={handleLeafClose}
                  inline={leafInline}
                  style={{ top: leafPosition.top, left: leafPosition.left }}
                  onMouseEnter={cancelLeafClose}
                  onMouseLeave={scheduleLeafClose}
                />
              )}
            </>
          )}
          <button
            type="button"
            role="menuitem"
            className="row-actions__item"
            onClick={handleToggleHidden}
            disabled={hiddenPending}
          >
            {hiddenPending ? 'Updating…' : pr.hidden ? '👁 Unhide' : '🙈 Hide'}
          </button>
          <button
            type="button"
            role="menuitem"
            className="row-actions__item row-actions__item--danger"
            onClick={handleDelete}
            disabled={deletePending}
          >
            {deletePending ? 'Deleting…' : '🗑 Delete'}
          </button>
        </div>,
        document.body
      )}

      {dialogAction && quickActions && (
        <QuickActionDialog
          pr={pr}
          action={dialogAction}
          login={user?.github_username}
          onSubmit={handleDialogSubmit}
          pending={quickActions.pending}
          error={quickActions.error}
          headMovedTo={headMovedTo}
          onClose={closeDialog}
          returnFocusRef={anchorRef}
        />
      )}
    </>
  );
}
