import { memo, useEffect, useId } from 'react';
import { createPortal } from 'react-dom';
import { useDropdown } from '@/hooks/useDropdown';
import { useTooltipIntent } from '@/hooks/useTooltipIntent';
import './IconTooltip.scss';

interface IconTooltipProps {
  label: string;
  className: string;
  children: string;
}

// The browser's own title tooltip waits about a second, long enough that a
// status icon reads as unexplained.
const HOVER_DELAY_MS = 300;
// Keep in sync with max-width in IconTooltip.scss.
const TOOLTIP_MAX_WIDTH = 260;
const VIEWPORT_MARGIN = 8;

export const IconTooltip = memo(function IconTooltip({ label, className, children }: IconTooltipProps) {
  const tooltipId = useId();
  const intent = useTooltipIntent(HOVER_DELAY_MS);
  const { isOpen, open, close, anchorRef, panelRef, position } = useDropdown({
    panelWidth: Math.min(TOOLTIP_MAX_WIDTH, window.innerWidth - 2 * VIEWPORT_MARGIN),
    align: 'left',
    viewportMargin: VIEWPORT_MARGIN,
  });

  useEffect(() => {
    if (intent.open) open();
    else close();
  }, [intent.open, open, close]);

  const dismiss = intent.dismiss;
  useEffect(() => {
    if (!intent.open) return;
    const onDocKeyDown = (e: KeyboardEvent) => {
      if (e.key === 'Escape') dismiss();
    };
    document.addEventListener('keydown', onDocKeyDown);
    return () => document.removeEventListener('keydown', onDocKeyDown);
  }, [intent.open, dismiss]);

  return (
    <>
      <span
        ref={anchorRef as React.RefObject<HTMLSpanElement>}
        className={className}
        role="img"
        aria-label={label}
        tabIndex={0}
        onMouseEnter={intent.onMouseEnter}
        onMouseLeave={intent.onMouseLeave}
        onFocus={intent.onFocus}
        onBlur={intent.onBlur}
      >
        {children}
      </span>
      {isOpen &&
        createPortal(
          <div
            id={tooltipId}
            ref={panelRef}
            role="tooltip"
            className="icon-tooltip"
            style={{ top: position.top, left: position.left }}
          >
            {label}
          </div>,
          document.body
        )}
    </>
  );
});
