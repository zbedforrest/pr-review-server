// Keep in sync with $leaf-width in QuickActionsSubmenu.scss.
export const QUICK_ACTIONS_LEAF_WIDTH = 240;

/** Items arrow keys can land on: aria-disabled ones stay reachable so their reason is announced. */
export function roveableMenuItems(root: HTMLElement | null): HTMLElement[] {
  if (!root) return [];
  return Array.from(root.querySelectorAll<HTMLElement>('[role="menuitem"]')).filter(
    (el) => !el.hasAttribute('disabled')
  );
}

/** Items that can be activated; the first of these takes focus when a menu opens. */
export function focusableMenuItems(root: HTMLElement | null): HTMLElement[] {
  return roveableMenuItems(root).filter((el) => el.getAttribute('aria-disabled') !== 'true');
}

/** Moves focus among enabled menu items for ArrowUp/Down/Home/End. Returns true when handled. */
export function roveMenuFocus(items: HTMLElement[], key: string, current: Element | null): boolean {
  if (items.length === 0) return false;
  const index = items.findIndex((el) => el === current);
  let next: number;
  switch (key) {
    case 'ArrowDown':
      next = index < 0 ? 0 : (index + 1) % items.length;
      break;
    case 'ArrowUp':
      next = index < 0 ? items.length - 1 : (index - 1 + items.length) % items.length;
      break;
    case 'Home':
      next = 0;
      break;
    case 'End':
      next = items.length - 1;
      break;
    default:
      return false;
  }
  items[next].focus();
  return true;
}
