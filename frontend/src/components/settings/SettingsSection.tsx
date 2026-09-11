import type { ReactNode } from 'react';
import './settings.scss';

interface SettingsSectionProps {
  title: string;
  description?: string;
  dirty: boolean;
  canSave: boolean;
  saving: boolean;
  saved?: boolean;
  error?: string;
  onSave: () => void;
  onReset: () => void;
  children: ReactNode;
}

export function SettingsSection({
  title,
  description,
  dirty,
  canSave,
  saving,
  saved = false,
  error,
  onSave,
  onReset,
  children,
}: SettingsSectionProps) {
  return (
    <section className="settings-section" aria-busy={saving}>
      <div className="settings-section__header">
        <h2 className="settings-section__title">{title}</h2>
        {description && <p className="settings-section__description">{description}</p>}
      </div>
      <div className="settings-section__body">{children}</div>
      <div className="settings-section__actions">
        <span role="status" className="settings-section__status">
          {saving ? 'Saving' : saved ? 'Saved' : ''}
        </span>
        <button
          type="button"
          className="settings-section__button"
          onClick={onReset}
          disabled={!dirty}
        >
          Reset
        </button>
        <button
          type="button"
          className="settings-section__button settings-section__button--primary"
          onClick={onSave}
          disabled={!dirty || !canSave || saving}
        >
          Save
        </button>
      </div>
      {error && (
        <div role="alert" className="settings-section__error">
          {error}
        </div>
      )}
    </section>
  );
}
