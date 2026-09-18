import { act, cleanup, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { APIError } from '@/api/client';
import type { PR } from '@/types/pr';
import { LEAF_CLOSE_DELAY_MS, LEAF_OPEN_DELAY_MS, RowActionsMenu, type QuickActionsWiring } from './RowActionsMenu';

const useTelemetryMock = vi.fn();
vi.mock('@/hooks/useTelemetry', () => ({
  useTelemetry: () => useTelemetryMock(),
}));

const makePR = (partial: Partial<PR> = {}): PR => ({
  owner: 'test-org',
  repo: 'test-repo',
  number: 1,
  commit_sha: 'abc123',
  last_reviewed_at: null,
  review_html_path: '',
  github_url: 'https://github.com/test-org/test-repo/pull/1',
  review_url: '',
  status: 'pending',
  title: 'Example PR',
  author: 'alice',
  generating_since: null,
  approval_count: 0,
  my_review_status: '',
  draft: false,
  ci_state: 'unknown',
  ci_failed_checks: [],
  created_at: '2026-04-15T12:00:00Z',
  is_mine: false,
  via_teams: [],
  critical_count: 0,
  medium_count: 0,
  low_count: 0,
  notes: '',
  ...partial,
});

const renderMenu = (overrides: Partial<React.ComponentProps<typeof RowActionsMenu>> = {}) => {
  const props = {
    pr: makePR(),
    onTriggerReview: vi.fn(),
    onToggleHidden: vi.fn(),
    onDelete: vi.fn(),
    reviewPending: false,
    hiddenPending: false,
    deletePending: false,
    publishAllowed: true,
    ...overrides,
  };
  render(<RowActionsMenu {...props} />);
  return props;
};

const openMenu = () => fireEvent.click(screen.getByRole('button', { name: /actions/i }));
const postItem = () => screen.getByRole('menuitem', { name: /generate and post pr comment/i }) as HTMLButtonElement;
const dashboardItem = () =>
  screen.getByRole('menuitem', { name: /generate review HTML only/i }) as HTMLButtonElement;

describe('RowActionsMenu', () => {
  beforeEach(() => {
    useTelemetryMock.mockReturnValue({ track: vi.fn() });
  });
  afterEach(() => cleanup());

  it('hides the menu until the trigger is clicked', () => {
    renderMenu();
    expect(screen.queryByRole('menu')).toBeNull();
    openMenu();
    expect(screen.queryByRole('menu')).toBeTruthy();
    expect(postItem()).toBeTruthy();
    expect(dashboardItem()).toBeTruthy();
    expect(screen.getByRole('menuitem', { name: /delete/i })).toBeTruthy();
  });

  it('labels the review items "Generate" when no review exists', () => {
    renderMenu({ pr: makePR() });
    openMenu();
    expect(postItem().textContent).toBe('🔄 Generate and post PR comment');
    expect(dashboardItem().textContent).toBe('🔄 Generate review HTML only');
  });

  it('labels the review items "Regenerate" once a review exists', () => {
    renderMenu({ pr: makePR({ review_url: '/reviews/x.html', status: 'completed' }) });
    openMenu();
    expect(postItem().textContent).toBe('🔄 Regenerate and post PR comment');
    expect(dashboardItem().textContent).toBe('🔄 Regenerate review HTML only');
  });

  it('calls onTriggerReview(true) and closes the menu from the post item', () => {
    const { onTriggerReview } = renderMenu();
    openMenu();
    fireEvent.click(postItem());
    expect(onTriggerReview).toHaveBeenCalledTimes(1);
    expect(onTriggerReview).toHaveBeenCalledWith(true);
    expect(screen.queryByRole('menu')).toBeNull();
  });

  it('calls onTriggerReview(false) and closes the menu from the dashboard-only item', () => {
    const { onTriggerReview } = renderMenu();
    openMenu();
    fireEvent.click(dashboardItem());
    expect(onTriggerReview).toHaveBeenCalledWith(false);
    expect(screen.queryByRole('menu')).toBeNull();
  });

  it('disables only the post item, with the pilot title, when the author is not in the pilot', () => {
    const { onTriggerReview } = renderMenu({ publishAllowed: false });
    openMenu();
    expect(postItem().disabled).toBe(true);
    expect(postItem().getAttribute('title')).toBe('Author is not in the comment pilot');
    expect(dashboardItem().disabled).toBe(false);
    fireEvent.click(postItem());
    expect(onTriggerReview).not.toHaveBeenCalled();
  });

  it('calls onDelete when Delete is clicked', () => {
    const { onDelete } = renderMenu();
    openMenu();
    fireEvent.click(screen.getByRole('menuitem', { name: /delete/i }));
    expect(onDelete).toHaveBeenCalledTimes(1);
  });

  it('disables both review items while a review is generating', () => {
    renderMenu({ pr: makePR({ status: 'generating' }) });
    openMenu();
    expect(postItem().disabled).toBe(true);
    expect(dashboardItem().disabled).toBe(true);
  });

  it('disables both review items while the trigger is pending', () => {
    renderMenu({ reviewPending: true });
    openMenu();
    expect(postItem().disabled).toBe(true);
    expect(dashboardItem().disabled).toBe(true);
  });

  it('never colors the kebab trigger by review status', () => {
    renderMenu({ pr: makePR({ status: 'agent_reviewing' }) });
    const trigger = screen.getByRole('button', { name: /actions/i });
    expect(trigger.className).toBe('row-actions__trigger');
    expect(trigger.getAttribute('title')).toBe('Row actions');
  });

  it('treats agent_reviewing as in-flight too', () => {
    renderMenu({ pr: makePR({ status: 'agent_reviewing' }) });
    openMenu();
    expect(postItem().disabled).toBe(true);
    expect(dashboardItem().disabled).toBe(true);
  });

  it('shows a Hide item for a visible PR', () => {
    renderMenu();
    openMenu();
    expect(screen.getByRole('menuitem', { name: /hide/i })).toBeTruthy();
    expect(screen.queryByRole('menuitem', { name: /unhide/i })).toBeNull();
  });

  it('shows an Unhide item for a hidden PR', () => {
    renderMenu({ pr: makePR({ hidden: true }) });
    openMenu();
    expect(screen.getByRole('menuitem', { name: /unhide/i })).toBeTruthy();
  });

  it('calls onToggleHidden and closes the menu when Hide is clicked', () => {
    const { onToggleHidden } = renderMenu();
    openMenu();
    fireEvent.click(screen.getByRole('menuitem', { name: /hide/i }));
    expect(onToggleHidden).toHaveBeenCalledTimes(1);
    expect(screen.queryByRole('menu')).toBeNull();
  });

  it('disables the hide item and shows "Updating…" while the toggle is pending', () => {
    renderMenu({ hiddenPending: true });
    openMenu();
    const item = screen.getByRole('menuitem', { name: /updating/i }) as HTMLButtonElement;
    expect(item.disabled).toBe(true);
  });

  it('shows "Deleting…" and disables Delete while a delete is pending', () => {
    renderMenu({ deletePending: true });
    openMenu();
    const item = screen.getByRole('menuitem', { name: /deleting/i }) as HTMLButtonElement;
    expect(item.disabled).toBe(true);
  });

  it('reflects open state via aria-expanded and closes on Escape', () => {
    renderMenu();
    const trigger = screen.getByRole('button', { name: /actions/i });
    expect(trigger.getAttribute('aria-haspopup')).toBe('menu');
    expect(trigger.getAttribute('aria-expanded')).toBe('false');
    fireEvent.click(trigger);
    expect(trigger.getAttribute('aria-expanded')).toBe('true');
    fireEvent.keyDown(document, { key: 'Escape' });
    expect(screen.queryByRole('menu')).toBeNull();
    expect(trigger.getAttribute('aria-expanded')).toBe('false');
  });
});

const quickUser = {
  id: 1,
  github_username: 'alice',
  github_avatar_url: '',
  is_admin: false,
  quick_actions_enabled: true,
  github_actions_available: true,
};

const makeWiring = (overrides: Partial<QuickActionsWiring> = {}): QuickActionsWiring => ({
  user: quickUser,
  onSubmit: vi.fn(() => new Promise(() => {})),
  onOpenGitHub: vi.fn(),
  onCopyLink: vi.fn(),
  onDialogClose: vi.fn(),
  pending: false,
  error: null,
  ...overrides,
});

const quickItem = () => screen.getByRole('menuitem', { name: /quick actions/i }) as HTMLButtonElement;
const queryQuickItem = () => screen.queryByRole('menuitem', { name: /quick actions/i });
const leaf = () => screen.queryByRole('menu', { name: 'Quick actions' });
const approveItem = () => screen.getByRole('menuitem', { name: /^Approve/ }) as HTMLButtonElement;

describe('RowActionsMenu quick actions', () => {
  let track: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    track = vi.fn();
    useTelemetryMock.mockReturnValue({ track });
  });
  afterEach(() => {
    cleanup();
    vi.useRealTimers();
  });

  it('renders no Quick actions item when the flag is off', () => {
    renderMenu({ quickActions: makeWiring({ user: { ...quickUser, quick_actions_enabled: false } }) });
    openMenu();
    expect(queryQuickItem()).toBeNull();
    expect(screen.getAllByRole('menuitem').map((el) => el.textContent)).toEqual([
      '🔄 Generate and post PR comment',
      '🔄 Generate review HTML only',
      '🙈 Hide',
      '🗑 Delete',
    ]);
    cleanup();
    renderMenu();
    openMenu();
    expect(queryQuickItem()).toBeNull();
  });

  it('places the item between the HTML-only and Hide items', () => {
    renderMenu({ quickActions: makeWiring() });
    openMenu();
    const labels = screen.getAllByRole('menuitem').map((el) => el.textContent?.replace(/\s+/g, ' ').trim());
    expect(labels).toEqual([
      '🔄 Generate and post PR comment',
      '🔄 Generate review HTML only',
      '⚡ Quick actions ▸',
      '🙈 Hide',
      '🗑 Delete',
    ]);
    expect(quickItem().getAttribute('aria-haspopup')).toBe('menu');
    expect(quickItem().getAttribute('aria-expanded')).toBe('false');
  });

  it('opens the quick actions leaf on click, inside the parent panel, with the footer login', () => {
    renderMenu({ quickActions: makeWiring() });
    openMenu();
    fireEvent.click(quickItem(), { detail: 1 });
    const panel = leaf();
    expect(panel).toBeTruthy();
    expect(quickItem().getAttribute('aria-expanded')).toBe('true');
    expect(screen.getAllByRole('menu')[0].contains(panel)).toBe(true);
    expect(panel!.textContent).toContain('acts as @alice on GitHub');
    expect(track).toHaveBeenCalledWith('quick_actions_open', expect.objectContaining({ label: 'click' }));
    fireEvent.click(quickItem());
    expect(leaf()).toBeNull();
  });

  it('opens the leaf after hover delay and closes after leave delay', () => {
    vi.useFakeTimers();
    renderMenu({ quickActions: makeWiring() });
    openMenu();
    fireEvent.mouseEnter(quickItem());
    act(() => {
      vi.advanceTimersByTime(LEAF_OPEN_DELAY_MS - 1);
    });
    expect(leaf()).toBeNull();
    act(() => {
      vi.advanceTimersByTime(1);
    });
    expect(leaf()).toBeTruthy();
    expect(track).toHaveBeenCalledWith('quick_actions_open', expect.objectContaining({ label: 'hover' }));

    fireEvent.mouseLeave(quickItem());
    fireEvent.mouseEnter(leaf()!);
    act(() => {
      vi.advanceTimersByTime(LEAF_CLOSE_DELAY_MS + 50);
    });
    expect(leaf()).toBeTruthy();

    fireEvent.mouseLeave(leaf()!);
    act(() => {
      vi.advanceTimersByTime(LEAF_CLOSE_DELAY_MS - 1);
    });
    expect(leaf()).toBeTruthy();
    act(() => {
      vi.advanceTimersByTime(1);
    });
    expect(leaf()).toBeNull();
  });

  it('ArrowRight opens the leaf and focuses its first item', () => {
    renderMenu({ quickActions: makeWiring() });
    openMenu();
    quickItem().focus();
    fireEvent.keyDown(quickItem(), { key: 'ArrowRight' });
    expect(leaf()).toBeTruthy();
    expect(document.activeElement).toBe(approveItem());
    expect(track).toHaveBeenCalledWith('quick_actions_open', expect.objectContaining({ label: 'keyboard' }));
  });

  it('Escape in the leaf closes only the leaf and refocuses the item', () => {
    renderMenu({ quickActions: makeWiring() });
    openMenu();
    fireEvent.click(quickItem(), { detail: 0 });
    expect(leaf()).toBeTruthy();
    expect(document.activeElement).toBe(approveItem());
    expect(track).toHaveBeenCalledWith('quick_actions_open', expect.objectContaining({ label: 'keyboard' }));
    fireEvent.keyDown(approveItem(), { key: 'Escape' });
    expect(leaf()).toBeNull();
    expect(screen.queryAllByRole('menu')).toHaveLength(1);
    expect(document.activeElement).toBe(quickItem());
    fireEvent.click(quickItem(), { detail: 0 });
    fireEvent.keyDown(approveItem(), { key: 'ArrowLeft' });
    expect(leaf()).toBeNull();
    expect(screen.queryAllByRole('menu')).toHaveLength(1);
  });

  it('clicking inside the leaf does not close the parent menu', () => {
    renderMenu({ quickActions: makeWiring() });
    openMenu();
    fireEvent.click(quickItem());
    fireEvent.mouseDown(screen.getByRole('menuitem', { name: /Copy link/ }));
    expect(screen.queryAllByRole('menu')).toHaveLength(2);
    fireEvent.mouseDown(document.body);
    expect(screen.queryAllByRole('menu')).toHaveLength(0);
  });

  it('choosing Approve closes the menus and opens the dialog', () => {
    const wiring = makeWiring();
    renderMenu({ quickActions: wiring });
    openMenu();
    fireEvent.click(quickItem());
    fireEvent.click(approveItem());
    expect(screen.queryAllByRole('menu')).toHaveLength(0);
    const dialog = screen.getByRole('dialog');
    expect(dialog.textContent).toContain('Approve test-org/test-repo #1');
    fireEvent.click(screen.getByRole('button', { name: /^Approve$/ }));
    expect(wiring.onSubmit).toHaveBeenCalledWith('approve', '', 'abc123');
  });

  it('closes the dialog when the submission resolves and resets the owner state', async () => {
    const wiring = makeWiring({ onSubmit: vi.fn(() => Promise.resolve({})) });
    renderMenu({ quickActions: wiring });
    openMenu();
    fireEvent.click(quickItem());
    fireEvent.click(screen.getByRole('menuitem', { name: /Comment/ }));
    fireEvent.change(screen.getByRole('textbox'), { target: { value: 'nit' } });
    fireEvent.click(screen.getByRole('button', { name: /^Comment$/ }));
    await act(async () => {});
    expect(screen.queryByRole('dialog')).toBeNull();
    expect(wiring.onDialogClose).toHaveBeenCalledTimes(1);
  });

  it('keeps the dialog open on rejection and shows the head-moved state from the error prop', () => {
    const error = new APIError('moved', 409, 'Conflict', 'head_moved', { head_sha: 'def5678' });
    const wiring = makeWiring({ onSubmit: vi.fn(() => Promise.reject(error)), error });
    renderMenu({ quickActions: wiring });
    openMenu();
    fireEvent.click(quickItem());
    fireEvent.click(approveItem());
    expect(screen.getByRole('dialog').textContent).toContain('The PR head moved from abc123 to def5678');
    expect(screen.getByRole('button', { name: 'Approve def5678' })).toBeTruthy();
  });

  it('shows a plain fetch failure in the dialog', () => {
    const error = new TypeError('Failed to fetch');
    renderMenu({ quickActions: makeWiring({ onSubmit: vi.fn(() => Promise.reject(error)), error }) });
    openMenu();
    fireEvent.click(quickItem());
    fireEvent.click(approveItem());
    expect(screen.getByRole('alert').textContent).toContain('GitHub returned an error: Failed to fetch');
  });

  it('disabled Approve carries the own-PR title', () => {
    renderMenu({ pr: makePR({ is_mine: true }), quickActions: makeWiring() });
    openMenu();
    fireEvent.click(quickItem());
    expect(approveItem().getAttribute('aria-disabled')).toBe('true');
    expect(approveItem().getAttribute('title')).toBe('You cannot approve your own PR');
    expect(document.getElementById(approveItem().getAttribute('aria-describedby')!)?.textContent).toBe('You cannot approve your own PR');
    expect(screen.getByRole('menuitem', { name: /Comment/ }).getAttribute('aria-disabled')).toBe('false');
    fireEvent.click(approveItem());
    expect(screen.queryByRole('dialog')).toBeNull();
    expect(screen.queryAllByRole('menu')).toHaveLength(2);
  });

  it('offers sign-in and disables review items when the token is unavailable', () => {
    renderMenu({ quickActions: makeWiring({ user: { ...quickUser, github_actions_available: false } }) });
    openMenu();
    fireEvent.click(quickItem());
    expect(approveItem().getAttribute('aria-disabled')).toBe('true');
    expect(approveItem().getAttribute('title')).toBe('Sign in again to enable GitHub actions');
    const signIn = screen.getByRole('menuitem', { name: /Sign in again to enable/ });
    expect(signIn.getAttribute('href')).toBe('/login');
    expect(screen.getByRole('menuitem', { name: /Open on GitHub/ }).getAttribute('href')).toBe(
      'https://github.com/test-org/test-repo/pull/1'
    );
  });

  it('keeps aria-disabled leaf items in the arrow order but not as the initial focus', () => {
    renderMenu({ pr: makePR({ is_mine: true }), quickActions: makeWiring() });
    openMenu();
    fireEvent.keyDown(quickItem(), { key: 'ArrowRight' });
    const comment = screen.getByRole('menuitem', { name: /Comment/ });
    expect(document.activeElement).toBe(comment);
    fireEvent.keyDown(leaf()!, { key: 'ArrowUp' });
    expect(document.activeElement).toBe(screen.getByRole('menuitem', { name: /Request changes/ }));
    fireEvent.keyDown(leaf()!, { key: 'ArrowUp' });
    expect(document.activeElement).toBe(approveItem());
  });

  it('roves focus through the parent items with arrow keys and skips leaf items', () => {
    renderMenu({ quickActions: makeWiring() });
    const trigger = screen.getByRole('button', { name: /actions/i });
    fireEvent.keyDown(trigger, { key: 'Enter' });
    fireEvent.click(trigger);
    expect(document.activeElement).toBe(postItem());
    const panel = screen.getByRole('menu');
    fireEvent.keyDown(panel, { key: 'ArrowDown' });
    expect(document.activeElement).toBe(dashboardItem());
    fireEvent.keyDown(panel, { key: 'End' });
    expect(document.activeElement).toBe(screen.getByRole('menuitem', { name: /delete/i }));
    fireEvent.keyDown(panel, { key: 'ArrowDown' });
    expect(document.activeElement).toBe(postItem());
    fireEvent.keyDown(panel, { key: 'ArrowUp' });
    expect(document.activeElement).toBe(screen.getByRole('menuitem', { name: /delete/i }));
    fireEvent.keyDown(document, { key: 'Escape' });
    expect(document.activeElement).toBe(trigger);
  });
});
