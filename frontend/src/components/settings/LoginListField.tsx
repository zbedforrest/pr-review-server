import { useEffect, useMemo, useRef, useState } from 'react';
import { joinLogins, normalizeLogins } from './loginList';
import './settings.scss';

interface LoginListFieldProps {
  id: string;
  label: string;
  // Comma-separated logins as the server stores them.
  value: string;
  onChange: (next: string) => void;
  authors: boolean;
  disabled: boolean;
  fixed?: string[];
  knownLogins?: Set<string>;
}

const UNKNOWN_HINT = 'not seen on any PR';

export function LoginListField({
  id,
  label,
  value,
  onChange,
  authors,
  disabled,
  fixed = [],
  knownLogins,
}: LoginListFieldProps) {
  const [draft, setDraft] = useState('');
  const [error, setError] = useState<string | null>(null);
  const inputRef = useRef<HTMLInputElement>(null);
  const focusAfterRemove = useRef<string | null>(null);
  const logins = normalizeLogins(value, authors).logins;
  const fixedSet = new Set(fixed.map((login) => login.trim().toLowerCase()));
  const editable = logins.filter((login) => !fixedSet.has(login));

  useEffect(() => {
    const removed = focusAfterRemove.current;
    focusAfterRemove.current = null;
    if (removed !== null && !logins.includes(removed)) inputRef.current?.focus();
  }, [logins]);

  const commit = (raw: string) => {
    const entry = normalizeLogins(raw, authors);
    if (entry.invalid.length > 0) {
      setError(`"${entry.invalid[0]}" is not a valid login`);
      return;
    }
    setError(null);
    const added = entry.logins.filter((login) => !logins.includes(login));
    if (added.length === 0) {
      setDraft('');
      return;
    }
    if (added.includes('*') && !window.confirm('Publish for every author?')) {
      setDraft('');
      return;
    }
    onChange(joinLogins([...logins, ...added]));
    setDraft('');
  };

  const handleChange = (e: React.ChangeEvent<HTMLInputElement>) => {
    const next = e.target.value;
    setDraft(next);
    if (next.includes(',')) commit(next);
  };

  // A text input drops newlines from its value, so a multi-line paste is turned into a comma list before it lands.
  const handlePaste = (e: React.ClipboardEvent<HTMLInputElement>) => {
    const pasted = e.clipboardData.getData('text');
    if (!/[,\n]/.test(pasted)) return;
    e.preventDefault();
    const { selectionStart, selectionEnd } = e.currentTarget;
    const start = selectionStart ?? draft.length;
    const end = selectionEnd ?? draft.length;
    const next = (draft.slice(0, start) + pasted + draft.slice(end)).replace(/\r?\n/g, ',');
    setDraft(next);
    commit(next);
  };

  const handleKeyDown = (e: React.KeyboardEvent<HTMLInputElement>) => {
    if (e.key === 'Enter') {
      e.preventDefault();
      if (draft.trim() !== '') commit(draft);
    }
  };

  const handleBlur = () => {
    if (draft.trim() !== '') commit(draft);
  };

  const remove = (login: string) => {
    focusAfterRemove.current = login;
    onChange(joinLogins(logins.filter((l) => l !== login)));
  };

  const known = useMemo(
    () => knownLogins && new Set([...knownLogins].map((login) => login.trim().toLowerCase())),
    [knownLogins],
  );
  const isUnknown = (login: string) => known !== undefined && login !== '*' && !known.has(login);

  const chipClass = (login: string, isFixed: boolean) =>
    [
      'settings-field__chip',
      isFixed && 'settings-field__chip--fixed',
      isUnknown(login) && 'settings-field__chip--unknown',
    ]
      .filter(Boolean)
      .join(' ');

  const errorId = `${id}-error`;
  const hintId = `${id}-hint`;
  const pending = draft.trim() !== '';
  const describedBy = [error && errorId, pending && hintId].filter(Boolean).join(' ') || undefined;

  return (
    <div className="settings-field">
      <label className="settings-field__label" htmlFor={id}>
        {label}
      </label>
      <div className="settings-field__chips">
        {fixed.map((login) => (
          <span key={`fixed:${login}`} className={chipClass(login, true)}>
            {login}
            <span className="settings-field__chip-hint">from deploy</span>
            {isUnknown(login) && <span className="settings-field__chip-hint">{UNKNOWN_HINT}</span>}
          </span>
        ))}
        {editable.map((login) => (
          <span key={login} className={chipClass(login, false)}>
            {login}
            {isUnknown(login) && <span className="settings-field__chip-hint">{UNKNOWN_HINT}</span>}
            {!disabled && (
              <button
                type="button"
                className="settings-field__chip-remove"
                aria-label={`Remove ${login}`}
                onClick={() => remove(login)}
              >
                ×
              </button>
            )}
          </span>
        ))}
        <input
          ref={inputRef}
          id={id}
          type="text"
          className="settings-field__input"
          value={draft}
          onChange={handleChange}
          onKeyDown={handleKeyDown}
          onPaste={handlePaste}
          onBlur={handleBlur}
          disabled={disabled}
          placeholder={disabled ? '' : 'Type a login and press Enter'}
          aria-invalid={error !== null}
          aria-describedby={describedBy}
          autoComplete="off"
          spellCheck={false}
        />
      </div>
      {pending && (
        <span id={hintId} className="settings-field__help">
          Press Enter to add
        </span>
      )}
      {error && (
        <span id={errorId} role="alert" className="settings-field__error">
          {error}
        </span>
      )}
    </div>
  );
}
