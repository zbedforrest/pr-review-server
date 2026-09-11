import { useCallback, useEffect, useLayoutEffect, useRef, useState } from 'react';

export interface DropdownPosition {
  top: number;
  left: number;
  placement: 'top' | 'bottom';
  /** Room between the panel and the viewport edge it grows toward, in px. */
  maxHeight: number;
}

export interface DropdownPositionOptions {
  /** Fixed width of the panel, in px. Used for horizontal clamping. */
  panelWidth: number;
  /**
   * Measured panel height, in px. When known, the panel flips above the anchor
   * if there isn't room below. Omit to always open downward.
   */
  panelHeight?: number;
  /** Which anchor edge the panel aligns to. Defaults to 'right'. */
  align?: 'left' | 'right';
  /** Gap between the anchor and the panel, in px. Defaults to 6. */
  gap?: number;
  /** Minimum distance to keep from the viewport edges, in px. Defaults to 8. */
  viewportMargin?: number;
}

/**
 * Pure positioning math for a fixed-position dropdown panel anchored to an
 * element. Kept separate from the hook so it can be unit-tested with
 * fabricated rects (jsdom's getBoundingClientRect returns zeros).
 */
export function computeDropdownPosition(
  anchor: Pick<DOMRect, 'top' | 'bottom' | 'left' | 'right'>,
  viewport: { width: number; height: number },
  opts: DropdownPositionOptions
): DropdownPosition {
  const { panelWidth, panelHeight, align = 'right', gap = 6, viewportMargin = 8 } = opts;

  // Horizontal: align to one anchor edge, then clamp inside the viewport.
  const rawLeft = align === 'right' ? anchor.right - panelWidth : anchor.left;
  const maxLeft = viewport.width - panelWidth - viewportMargin;
  const left = Math.max(viewportMargin, Math.min(rawLeft, maxLeft));

  // Vertical: prefer below; flip above only when below overflows, we know the
  // panel height, and there's actually room above.
  let placement: 'top' | 'bottom' = 'bottom';
  if (panelHeight != null) {
    const fitsBelow = anchor.bottom + gap + panelHeight + viewportMargin <= viewport.height;
    const fitsAbove = anchor.top - gap - panelHeight >= viewportMargin;
    if (!fitsBelow && fitsAbove) placement = 'top';
  }
  const top = placement === 'bottom'
    ? anchor.bottom + gap
    : anchor.top - gap - (panelHeight ?? 0);
  const maxHeight = placement === 'bottom'
    ? viewport.height - top - viewportMargin
    : anchor.top - gap - viewportMargin;

  return { top, left, placement, maxHeight };
}

export interface UseDropdownOptions extends DropdownPositionOptions {
  /** Close when a mousedown lands outside both the anchor and the panel. */
  closeOnOutsideClick?: boolean;
  /** Close when Escape is pressed. */
  closeOnEscape?: boolean;
}

export interface UseDropdownResult {
  isOpen: boolean;
  open: () => void;
  close: () => void;
  toggle: () => void;
  anchorRef: React.RefObject<HTMLElement>;
  panelRef: React.RefObject<HTMLDivElement>;
  position: DropdownPosition;
}

/**
 * Drives an anchored, portal-friendly dropdown panel: open/close state, fixed
 * positioning relative to the anchor (with viewport clamping and optional
 * flip-up), reflow on scroll/resize, and optional Escape / outside-click
 * dismissal. Trigger semantics (hover vs click) live in the consumer.
 */
export function useDropdown(options: UseDropdownOptions): UseDropdownResult {
  const {
    panelWidth,
    panelHeight,
    align,
    gap,
    viewportMargin,
    closeOnOutsideClick = false,
    closeOnEscape = false,
  } = options;

  const anchorRef = useRef<HTMLElement>(null);
  const panelRef = useRef<HTMLDivElement>(null);
  const [isOpen, setIsOpen] = useState(false);
  const [position, setPosition] = useState<DropdownPosition>({
    top: 0,
    left: 0,
    placement: 'bottom',
    maxHeight: window.innerHeight,
  });

  const open = useCallback(() => setIsOpen(true), []);
  const close = useCallback(() => setIsOpen(false), []);
  const toggle = useCallback(() => setIsOpen((v) => !v), []);

  const recompute = useCallback(() => {
    const el = anchorRef.current;
    if (!el) return;
    const anchor = el.getBoundingClientRect();
    // Prefer the live panel height (for flip-up) but fall back to the caller's.
    // Adding back the overflow hidden by a max-height clamp keeps the flip
    // decision on the natural height, so a clamped panel does not stay put.
    const panel = panelRef.current;
    const measured = panel
      ? panel.getBoundingClientRect().height + panel.scrollHeight - panel.clientHeight
      : undefined;
    setPosition(
      computeDropdownPosition(
        anchor,
        { width: window.innerWidth, height: window.innerHeight },
        { panelWidth, panelHeight: measured || panelHeight, align, gap, viewportMargin }
      )
    );
  }, [panelWidth, panelHeight, align, gap, viewportMargin]);

  // Recompute once the panel has mounted so flip-up can use its real height.
  useLayoutEffect(() => {
    if (isOpen) recompute();
  }, [isOpen, recompute]);

  // Keep the panel glued to the anchor while open.
  useEffect(() => {
    if (!isOpen) return;
    const onReflow = () => recompute();
    window.addEventListener('scroll', onReflow, true);
    window.addEventListener('resize', onReflow);
    return () => {
      window.removeEventListener('scroll', onReflow, true);
      window.removeEventListener('resize', onReflow);
    };
  }, [isOpen, recompute]);

  // Escape to close.
  useEffect(() => {
    if (!isOpen || !closeOnEscape) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') setIsOpen(false);
    };
    document.addEventListener('keydown', onKey);
    return () => document.removeEventListener('keydown', onKey);
  }, [isOpen, closeOnEscape]);

  // Outside mousedown to close.
  useEffect(() => {
    if (!isOpen || !closeOnOutsideClick) return;
    const onDown = (e: MouseEvent) => {
      const target = e.target as Node;
      if (anchorRef.current?.contains(target) || panelRef.current?.contains(target)) return;
      setIsOpen(false);
    };
    document.addEventListener('mousedown', onDown);
    return () => document.removeEventListener('mousedown', onDown);
  }, [isOpen, closeOnOutsideClick]);

  return { isOpen, open, close, toggle, anchorRef, panelRef, position };
}
