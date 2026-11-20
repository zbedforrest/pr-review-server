import { useCallback, useEffect, useRef, useState } from 'react';

/**
 * Hover and focus are independent reasons to show a tooltip: leaving with
 * the pointer must not close a tooltip the keyboard opened, and vice versa.
 */
export function useTooltipIntent(hoverDelayMs: number) {
  const [hovered, setHovered] = useState(false);
  const [focused, setFocused] = useState(false);
  const timer = useRef<ReturnType<typeof setTimeout>>();
  const cancel = useCallback(() => {
    if (timer.current) clearTimeout(timer.current);
    timer.current = undefined;
  }, []);

  useEffect(() => cancel, [cancel]);

  return {
    open: hovered || focused,
    onMouseEnter: useCallback(() => {
      cancel();
      timer.current = setTimeout(() => setHovered(true), hoverDelayMs);
    }, [cancel, hoverDelayMs]),
    onMouseLeave: useCallback(() => {
      cancel();
      setHovered(false);
    }, [cancel]),
    onFocus: useCallback(() => setFocused(true), []),
    onBlur: useCallback(() => setFocused(false), []),
    dismiss: useCallback(() => {
      cancel();
      setHovered(false);
      setFocused(false);
    }, [cancel]),
  };
}
