import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
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

  it('asks before adding "*" and leaves the value intact when cancelled', () => {
    vi.mocked(window.confirm).mockReturnValue(false);
    const { onChange } = renderField();
    type('*');
    fireEvent.keyDown(input(), { key: 'Enter' });
    expect(window.confirm).toHaveBeenCalledWith('Publish for every author?');
    expect(onChange).not.toHaveBeenCalled();
    expect(screen.queryByRole('alert')).toBeNull();
  });

  it('adds "*" when the confirm is accepted', () => {
    const { onChange } = renderField();
    type('*');
    fireEvent.keyDown(input(), { key: 'Enter' });
    expect(window.confirm).toHaveBeenCalledWith('Publish for every author?');
    expect(onChange).toHaveBeenCalledWith('alice,bob,*');
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

  it('shows no hint when knownLogins is not provided', () => {
    renderField();
    expect(screen.queryByText('not seen on any PR')).toBeNull();
  });
});
