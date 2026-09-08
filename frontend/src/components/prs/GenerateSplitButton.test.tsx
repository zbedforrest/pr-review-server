import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { GenerateSplitButton } from './GenerateSplitButton';

const renderButton = (overrides: Partial<React.ComponentProps<typeof GenerateSplitButton>> = {}) => {
  const props = {
    onGenerate: vi.fn(),
    pending: false,
    publishAllowed: true,
    ...overrides,
  };
  render(<GenerateSplitButton {...props} />);
  return props;
};

const primary = () => screen.getByRole('button', { name: /^🔄 Generate$/ }) as HTMLButtonElement;
const chevron = () => screen.getByRole('button', { name: 'More generate options' }) as HTMLButtonElement;
const openMenu = () => fireEvent.click(chevron());
const postItem = () => screen.getByRole('menuitem', { name: /generate and post to pr/i }) as HTMLButtonElement;
const dashboardItem = () => screen.getByRole('menuitem', { name: /generate for dashboard only/i }) as HTMLButtonElement;

describe('GenerateSplitButton', () => {
  afterEach(() => cleanup());

  it('renders a primary Generate segment that posts to the PR by default', () => {
    const { onGenerate } = renderButton();
    expect(primary().getAttribute('title')).toBe('Generate an AI review and post it to the PR');
    fireEvent.click(primary());
    expect(onGenerate).toHaveBeenCalledTimes(1);
    expect(onGenerate).toHaveBeenCalledWith(true);
  });

  it('shows "Starting…" and disables both segments while pending', () => {
    renderButton({ pending: true });
    const btn = screen.getByRole('button', { name: /starting/i }) as HTMLButtonElement;
    expect(btn.disabled).toBe(true);
    expect(chevron().disabled).toBe(true);
    expect(btn.parentElement?.className).toContain('generate-split--pending');
    expect(screen.queryByRole('button', { name: /^🔄 Generate$/ })).toBeNull();
  });

  it('exposes the chevron as a menu trigger and hides the menu until clicked', () => {
    renderButton();
    const trigger = chevron();
    expect(trigger.getAttribute('aria-haspopup')).toBe('menu');
    expect(trigger.getAttribute('aria-expanded')).toBe('false');
    expect(screen.queryByRole('menu')).toBeNull();
    openMenu();
    expect(trigger.getAttribute('aria-expanded')).toBe('true');
    expect(screen.getByRole('menu')).toBeTruthy();
  });

  it('lists both options with a title and a one-line description', () => {
    renderButton();
    openMenu();
    expect(postItem().textContent).toContain('Generate and post to PR');
    expect(postItem().textContent).toContain('Summary and inline comments as the Prism bot');
    expect(dashboardItem().textContent).toContain('Generate for dashboard only');
    expect(dashboardItem().textContent).toContain('Nothing is posted to GitHub');
  });

  it('calls onGenerate(true) from the post item and closes the menu', () => {
    const { onGenerate } = renderButton();
    openMenu();
    fireEvent.click(postItem());
    expect(onGenerate).toHaveBeenCalledWith(true);
    expect(screen.queryByRole('menu')).toBeNull();
  });

  it('calls onGenerate(false) from the dashboard-only item and closes the menu', () => {
    const { onGenerate } = renderButton();
    openMenu();
    fireEvent.click(dashboardItem());
    expect(onGenerate).toHaveBeenCalledWith(false);
    expect(screen.queryByRole('menu')).toBeNull();
  });

  it('closes on Escape and on an outside mousedown', () => {
    renderButton();
    openMenu();
    fireEvent.keyDown(document, { key: 'Escape' });
    expect(screen.queryByRole('menu')).toBeNull();
    openMenu();
    fireEvent.mouseDown(document.body);
    expect(screen.queryByRole('menu')).toBeNull();
  });

  describe('when the author is outside the comment pilot', () => {
    it('makes the primary segment dashboard-only and says so in the title', () => {
      const { onGenerate } = renderButton({ publishAllowed: false });
      expect(primary().textContent).toBe('🔄 Generate');
      expect(primary().getAttribute('title')).toBe(
        'Generate an AI review (dashboard only; author is not in the comment pilot)'
      );
      fireEvent.click(primary());
      expect(onGenerate).toHaveBeenCalledWith(false);
    });

    it('disables the post item with the pilot title and leaves dashboard-only usable', () => {
      const { onGenerate } = renderButton({ publishAllowed: false });
      openMenu();
      expect(postItem().disabled).toBe(true);
      expect(postItem().getAttribute('title')).toBe('Author is not in the comment pilot');
      fireEvent.click(postItem());
      expect(onGenerate).not.toHaveBeenCalled();
      fireEvent.click(dashboardItem());
      expect(onGenerate).toHaveBeenCalledWith(false);
    });
  });
});
