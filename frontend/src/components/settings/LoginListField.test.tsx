import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { useState } from 'react';
import { LoginListField } from './LoginListField';

const LABEL = 'Publish for authors';

const renderField = (overrides: Partial<React.ComponentProps<typeof LoginListField>> = {}) => {
  const props = {
    id: 'publish-authors',
    label: LABEL,
    value: 'alice,bob',
    onChange: vi.fn(),
    authors: true,
    disabled: false,
    ...overrides,
  };
  render(<LoginListField {...props} />);
  return props;
};

const input = () => screen.getByLabelText(LABEL) as HTMLInputElement;
const type = (text: string) => fireEvent.change(input(), { target: { value: text } });
const removeButtons = () => screen.queryAllByRole('button', { name: /^Remove / });

describe('LoginListField', () => {
  beforeEach(() => {
    vi.spyOn(window, 'confirm').mockReturnValue(true);
  });
  afterEach(() => {
    cleanup();
    vi.restoreAllMocks();
  });

  it('renders one chip per login with a remove button', () => {
    renderField();
    expect(screen.getByText('alice')).toBeTruthy();
    expect(screen.getByText('bob')).toBeTruthy();
    expect(screen.getByRole('button', { name: 'Remove alice' })).toBeTruthy();
    expect(screen.getByRole('button', { name: 'Remove bob' })).toBeTruthy();
  });

  it('adds the typed login on Enter and clears the input', () => {
    const { onChange } = renderField();
    type('Carol');
    fireEvent.keyDown(input(), { key: 'Enter' });
    expect(onChange).toHaveBeenCalledWith('alice,bob,carol');
    expect(input().value).toBe('');
  });

  it('adds the typed login when a comma is typed', () => {
    const { onChange } = renderField();
    type('carol,');
    expect(onChange).toHaveBeenCalledWith('alice,bob,carol');
    expect(input().value).toBe('');
  });

  it('adds every login from a pasted comma list, deduping against the current value', () => {
    const { onChange } = renderField();
    type('carol, alice ,dave');
    expect(onChange).toHaveBeenCalledWith('alice,bob,carol,dave');
  });

  it('adds every login from a pasted newline list', () => {
    const { onChange } = renderField();
    const paste = fireEvent.paste(input(), { clipboardData: { getData: () => 'carol\ndave\n' } });
    expect(paste).toBe(false);
    expect(onChange).toHaveBeenCalledWith('alice,bob,carol,dave');
    expect(input().value).toBe('');
  });

  it('appends a pasted list to what was already typed', () => {
    const { onChange } = renderField();
    type('car');
    fireEvent.paste(input(), { clipboardData: { getData: () => 'ol\ndave' } });
    expect(onChange).toHaveBeenCalledWith('alice,bob,carol,dave');
  });

  it('pastes over the selected text instead of appending to it', () => {
    const { onChange } = renderField();
    type('car');
    input().setSelectionRange(0, 3);
    fireEvent.paste(input(), { clipboardData: { getData: () => 'dave,erin' } });
    expect(onChange).toHaveBeenCalledWith('alice,bob,dave,erin');
  });

  it('pastes at the caret when the caret is inside the typed text', () => {
    const { onChange } = renderField();
    type('cl');
    input().setSelectionRange(1, 1);
    fireEvent.paste(input(), { clipboardData: { getData: () => 'aro,' } });
    expect(onChange).toHaveBeenCalledWith('alice,bob,caro,l');
  });

  it('keeps a rejected multi-line paste in the input as a comma list', () => {
    const { onChange } = renderField();
    fireEvent.paste(input(), { clipboardData: { getData: () => 'carol\nal_ice\ndave' } });
    expect(screen.getByRole('alert').textContent).toBe('"al_ice" is not a valid login');
    expect(input().value).toBe('carol,al_ice,dave');
    expect(onChange).not.toHaveBeenCalled();
  });

  it('rejects an invalid pasted entry inline and keeps the paste in the input', () => {
    const { onChange } = renderField();
    fireEvent.paste(input(), { clipboardData: { getData: () => 'carol,al ice' } });
    expect(screen.getByRole('alert').textContent).toBe('"al ice" is not a valid login');
    expect(input().value).toBe('carol,al ice');
    expect(onChange).not.toHaveBeenCalled();
  });

  it('lets a pasted single login go through the normal input flow', () => {
    const { onChange } = renderField();
    const paste = fireEvent.paste(input(), { clipboardData: { getData: () => 'carol' } });
    expect(paste).toBe(true);
    expect(onChange).not.toHaveBeenCalled();
  });

  it('tells the user to press Enter while a login is typed but not added', () => {
    renderField();
    expect(input().placeholder).toBe('Type a login and press Enter');
    expect(screen.queryByText('Press Enter to add')).toBeNull();
    type('carol');
    const hint = screen.getByText('Press Enter to add');
    expect(input().getAttribute('aria-describedby')).toBe(hint.id);
    fireEvent.keyDown(input(), { key: 'Enter' });
    expect(screen.queryByText('Press Enter to add')).toBeNull();
    expect(input().getAttribute('aria-describedby')).toBeNull();
  });

  it('describes the input by both the hint and the error when an entry was rejected', () => {
    renderField();
    type('al ice');
    fireEvent.keyDown(input(), { key: 'Enter' });
    const ids = input().getAttribute('aria-describedby')!.split(' ');
    expect(ids).toContain(screen.getByRole('alert').id);
    expect(ids).toContain(screen.getByText('Press Enter to add').id);
  });

  it('adds the typed login on blur', () => {
    const { onChange } = renderField();
    type('carol');
    fireEvent.blur(input());
    expect(onChange).toHaveBeenCalledWith('alice,bob,carol');
  });

  it('rejects an invalid entry inline and keeps it in the input', () => {
    const { onChange } = renderField();
    type('al ice');
    fireEvent.keyDown(input(), { key: 'Enter' });
    expect(screen.getByRole('alert').textContent).toBe('"al ice" is not a valid login');
    expect(input().value).toBe('al ice');
    expect(onChange).not.toHaveBeenCalled();
  });

  it('clears the error once a valid entry is added', () => {
    renderField();
    type('-bob');
    fireEvent.keyDown(input(), { key: 'Enter' });
    expect(screen.getByRole('alert')).toBeTruthy();
    type('carol');
    fireEvent.keyDown(input(), { key: 'Enter' });
    expect(screen.queryByRole('alert')).toBeNull();
  });

  it('rejects [bot] authors and "*" when the list is for admins', () => {
    const { onChange } = renderField({ authors: false });
    type('dependabot[bot]');
    fireEvent.keyDown(input(), { key: 'Enter' });
    expect(screen.getByRole('alert').textContent).toBe('"dependabot[bot]" is not a valid login');
    type('*');
    fireEvent.keyDown(input(), { key: 'Enter' });
    expect(screen.getByRole('alert').textContent).toBe('"*" is not a valid login');
    expect(onChange).not.toHaveBeenCalled();
    expect(window.confirm).not.toHaveBeenCalled();
  });

  it('removes a login when its chip button is clicked', () => {
    const { onChange } = renderField();
    fireEvent.click(screen.getByRole('button', { name: 'Remove alice' }));
    expect(onChange).toHaveBeenCalledWith('bob');
  });

  it('moves focus to the input after a chip is removed', () => {
    function Stateful() {
      const [value, setValue] = useState('alice,bob');
      return <LoginListField id="publish-authors" label={LABEL} value={value} onChange={setValue} authors disabled={false} />;
    }
    render(<Stateful />);
    const button = screen.getByRole('button', { name: 'Remove alice' });
    button.focus();
    fireEvent.click(button);
    expect(screen.queryByText('alice')).toBeNull();
    expect(document.activeElement).toBe(input());
  });

  it('leaves focus where it was when the parent keeps the login', () => {
    renderField({ onChange: vi.fn() });
    const button = screen.getByRole('button', { name: 'Remove alice' });
    button.focus();
    fireEvent.click(button);
    expect(document.activeElement).toBe(button);
  });

  it('does not steal focus later when a refused removal is followed by a server-side list change', () => {
    const field = (value: string) => (
      <LoginListField id="publish-authors" label={LABEL} value={value} onChange={vi.fn()} authors disabled={false} />
    );
    const { rerender } = render(field('alice,bob'));
    const button = screen.getByRole('button', { name: 'Remove alice' });
    button.focus();
    fireEvent.click(button);
    rerender(field('alice,bob,carol'));
    expect(document.activeElement).not.toBe(input());
  });

  it('asks before adding "*" and leaves the value intact when cancelled', () => {
    vi.mocked(window.confirm).mockReturnValue(false);
    const { onChange } = renderField();
    type('*');
    fireEvent.keyDown(input(), { key: 'Enter' });
    expect(window.confirm).toHaveBeenCalledWith('Publish for every author?');
    expect(onChange).not.toHaveBeenCalled();
    expect(screen.queryByRole('alert')).toBeNull();
    expect(input().value).toBe('');
    fireEvent.blur(input());
    expect(window.confirm).toHaveBeenCalledTimes(1);
  });

  it('does not ask again on later keystrokes after a cancelled "*" typed with a comma', () => {
    vi.mocked(window.confirm).mockReturnValue(false);
    const { onChange } = renderField();
    type('*,');
    expect(window.confirm).toHaveBeenCalledTimes(1);
    expect(input().value).toBe('');
    type('c');
    type('ca');
    expect(window.confirm).toHaveBeenCalledTimes(1);
    expect(onChange).not.toHaveBeenCalled();
  });

  it('adds "*" when the confirm is accepted', () => {
    const { onChange } = renderField();
    type('*');
    fireEvent.keyDown(input(), { key: 'Enter' });
    expect(window.confirm).toHaveBeenCalledWith('Publish for every author?');
    expect(onChange).toHaveBeenCalledWith('alice,bob,*');
  });

  it('shows a login that is both fixed and saved once, as the locked chip', () => {
    renderField({ value: 'owner,alice', fixed: ['owner'] });
    expect(screen.getAllByText('owner')).toHaveLength(1);
    expect(screen.queryByRole('button', { name: 'Remove owner' })).toBeNull();
    expect(screen.getByRole('button', { name: 'Remove alice' })).toBeTruthy();
  });

  it('renders fixed logins as locked chips without a remove button', () => {
    renderField({ value: '', fixed: ['owner'] });
    const chip = screen.getByText('owner').closest('.settings-field__chip') as HTMLElement;
    expect(chip.classList.contains('settings-field__chip--fixed')).toBe(true);
    expect(chip.textContent).toContain('from deploy');
    expect(removeButtons()).toHaveLength(0);
  });

  it('disables the input and hides remove buttons when disabled', () => {
    renderField({ disabled: true });
    expect(input().disabled).toBe(true);
    expect(screen.getByText('alice')).toBeTruthy();
    expect(removeButtons()).toHaveLength(0);
  });

  it('hints at logins not seen on any PR, and only those', () => {
    renderField({ value: 'alice,bob,*', knownLogins: new Set(['alice']) });
    const chipFor = (login: string) => screen.getByText(login).closest('.settings-field__chip') as HTMLElement;
    expect(chipFor('alice').classList.contains('settings-field__chip--unknown')).toBe(false);
    expect(chipFor('bob').classList.contains('settings-field__chip--unknown')).toBe(true);
    expect(chipFor('bob').textContent).toContain('not seen on any PR');
    expect(chipFor('*').classList.contains('settings-field__chip--unknown')).toBe(false);
    expect(screen.getAllByText('not seen on any PR')).toHaveLength(1);
  });

  it('matches known logins regardless of their casing', () => {
    renderField({ value: 'alice,bob', knownLogins: new Set(['Alice', ' BOB ']) });
    expect(screen.queryByText('not seen on any PR')).toBeNull();
  });

  it('shows no hint when knownLogins is not provided', () => {
    renderField();
    expect(screen.queryByText('not seen on any PR')).toBeNull();
  });
});
