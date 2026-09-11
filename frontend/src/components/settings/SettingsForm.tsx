import { useState } from 'react';
import type { ReplyMode, Settings } from '@/api/settings';
import { useUpdateSettings } from '@/hooks/useSettings';
import { LoginListField } from './LoginListField';
import { normalizeLogins } from './loginList';
import { SettingsSection } from './SettingsSection';
import './settings.scss';

export interface ReplyTotals {
  total: number;
  by_action: Record<string, number>;
  unlinked_roots?: number;
}

interface SettingsFormProps {
  settings: Settings;
  isAdmin: boolean;
  currentLogin: string;
  knownLogins?: Set<string>;
  replyTotals?: ReplyTotals | null;
}

const SEVERITIES = ['critical', 'medium', 'low'];

const REPLY_MODES: { value: ReplyMode; description: string }[] = [
  { value: 'off', description: 'Nothing is recorded or posted.' },
  { value: 'observe', description: 'Records author replies to inline comments.' },
  { value: 'react', description: 'Also acknowledges each reply with a thumbs-up reaction.' },
  {
    value: 'shadow',
    description: 'Also runs the reply model and records what it would say; still reacts, never posts text.',
  },
  { value: 'respond', description: 'Posts replies.' },
];

const EMPTY_AUTHORS_NOTICE =
  'Nothing is posted, and no replies are processed, until at least one author is enabled';

function pick<K extends keyof Settings>(settings: Settings, keys: readonly K[]): Pick<Settings, K> {
  return Object.fromEntries(keys.map((key) => [key, settings[key]])) as Pick<Settings, K>;
}

/**
 * Local draft for one section's keys. The draft follows the server object
 * whenever it changes, except for keys the user has edited and not yet saved,
 * and Save sends only the keys that differ from the server.
 */
function useSectionDraft<K extends keyof Settings>(settings: Settings, keys: readonly K[]) {
  type Draft = Pick<Settings, K>;
  const [base, setBase] = useState(settings);
  const [draft, setDraft] = useState<Draft>(() => pick(settings, keys));
  const [error, setError] = useState<string>();
  const [saved, setSaved] = useState(false);
  const update = useUpdateSettings();

  const follow = (from: Draft, to: Settings) =>
    setDraft((current) => {
      const next = { ...current };
      for (const key of keys) {
        if (current[key] === from[key]) next[key] = to[key];
      }
      return next;
    });

  if (settings !== base) {
    setBase(settings);
    follow(base, settings);
  }

  const changed = keys.filter((key) => draft[key] !== settings[key]);

  const save = async () => {
    const sent = draft;
    try {
      const response = await update.mutateAsync(pick({ ...settings, ...sent }, changed));
      setBase(response);
      follow(sent, response);
      setError(undefined);
      setSaved(true);
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    }
  };

  const reset = () => {
    setDraft(pick(settings, keys));
    setError(undefined);
  };

  const patch = (changes: Partial<Draft>) => {
    setDraft((current) => ({ ...current, ...changes }));
    setSaved(false);
    setError(undefined);
  };

  return {
    draft,
    patch,
    dirty: changed.length > 0,
    saving: update.isPending,
    saved: saved && changed.length === 0,
    error,
    save,
    reset,
  };
}

const numberValue = (n: number) => (Number.isNaN(n) ? '' : n);
const isCount = (n: number, min: number) => Number.isInteger(n) && n >= min;
const describedBy = (id: string, invalid: boolean) => (invalid ? `${id}-help ${id}-error` : `${id}-help`);

export function SettingsForm({ settings, isAdmin, currentLogin, knownLogins, replyTotals }: SettingsFormProps) {
  const disabled = !isAdmin;
  const review = useSectionDraft(settings, ['auto_review_requested_prs', 'review_n_requests']);
  const publishing = useSectionDraft(settings, [
    'publish_enabled_authors',
    'publish_inline_cap',
    'publish_inline_min_severity',
    'publish_show_unverified',
  ]);
  const replies = useSectionDraft(settings, ['publish_reply_mode']);
  const admins = useSectionDraft(settings, ['admin_logins']);

  const noAuthorsDraft = normalizeLogins(publishing.draft.publish_enabled_authors, true).logins.length === 0;
  const noAuthorsSaved = normalizeLogins(settings.publish_enabled_authors, true).logins.length === 0;
  const policyDisabled = disabled || noAuthorsDraft;
  const repliesDisabled = disabled || noAuthorsSaved;

  const chooseMode = (mode: ReplyMode) => {
    if (mode === 'respond' && !window.confirm('Post replies to authors on GitHub?')) return;
    replies.patch({ publish_reply_mode: mode });
  };

  const changeAdmins = (next: string) => {
    const me = currentLogin.trim().toLowerCase();
    const had = normalizeLogins(admins.draft.admin_logins, false).logins.includes(me);
    const keeps = normalizeLogins(next, false).logins.includes(me);
    const fixed = settings.admin_logins_fixed.some((login) => login.toLowerCase() === me);
    if (had && !keeps && !fixed && !window.confirm('Remove your own admin access?')) return;
    admins.patch({ admin_logins: next });
  };

  const samplesInvalid = !isCount(review.draft.review_n_requests, 1);
  const capInvalid = !isCount(publishing.draft.publish_inline_cap, 0);

  const adminList = [...settings.admin_logins_fixed, ...normalizeLogins(settings.admin_logins, false).logins];
  const activeSince = settings.publish_reply_enabled_at
    ? `Active since ${new Date(settings.publish_reply_enabled_at).toLocaleString()}`
    : 'Not active';

  return (
    <div className="settings-form">
      {disabled && (
        <p className="settings-section__notice">
          {adminList.length === 0
            ? 'Read only. No admins are configured; the deployer must set ADMIN_LOGINS on the server.'
            : `Read only. Admins: ${adminList.join(', ')}`}
        </p>
      )}

      <SettingsSection
        title="Review"
        description="Which PRs get reviewed and how hard the first pass works"
        dirty={review.dirty}
        canSave={isAdmin && !samplesInvalid}
        saving={review.saving}
        saved={review.saved}
        error={review.error}
        onSave={review.save}
        onReset={review.reset}
      >
        <div className="settings-field settings-field--inline">
          <input
            id="settings-auto-review"
            type="checkbox"
            checked={review.draft.auto_review_requested_prs}
            onChange={(e) => review.patch({ auto_review_requested_prs: e.target.checked })}
            disabled={disabled}
          />
          <label className="settings-field__label" htmlFor="settings-auto-review">
            Automatically review PRs that request a review from the app
          </label>
        </div>
        <div className="settings-field">
          <label className="settings-field__label" htmlFor="settings-review-n">
            First-pass samples
          </label>
          <input
            id="settings-review-n"
            type="number"
            className="settings-field__number"
            min={1}
            step={1}
            value={numberValue(review.draft.review_n_requests)}
            onChange={(e) => review.patch({ review_n_requests: e.target.valueAsNumber })}
            disabled={disabled}
            aria-invalid={samplesInvalid}
            aria-describedby={describedBy('settings-review-n', samplesInvalid)}
          />
          <span id="settings-review-n-help" className="settings-field__help">
            First-pass samples per review; multiplies LLM cost
          </span>
          {samplesInvalid && (
            <span id="settings-review-n-error" role="alert" className="settings-field__error">
              Enter a whole number of 1 or more
            </span>
          )}
        </div>
      </SettingsSection>

      <SettingsSection
        title="Publishing"
        description="Whose PRs receive posted reviews, and what gets posted"
        dirty={publishing.dirty}
        canSave={isAdmin && !capInvalid}
        saving={publishing.saving}
        saved={publishing.saved}
        error={publishing.error}
        onSave={publishing.save}
        onReset={publishing.reset}
      >
        <LoginListField
          id="settings-publish-authors"
          label="Publish for authors"
          value={publishing.draft.publish_enabled_authors}
          onChange={(next) => publishing.patch({ publish_enabled_authors: next })}
          authors
          disabled={disabled}
          knownLogins={knownLogins}
        />
        {noAuthorsDraft && <p className="settings-section__notice">{EMPTY_AUTHORS_NOTICE}</p>}
        <div className="settings-field">
          <label className="settings-field__label" htmlFor="settings-inline-cap">
            Inline comment cap
          </label>
          <input
            id="settings-inline-cap"
            type="number"
            className="settings-field__number"
            min={0}
            step={1}
            value={numberValue(publishing.draft.publish_inline_cap)}
            onChange={(e) => publishing.patch({ publish_inline_cap: e.target.valueAsNumber })}
            disabled={policyDisabled}
            aria-invalid={capInvalid}
            aria-describedby={describedBy('settings-inline-cap', capInvalid)}
          />
          <span id="settings-inline-cap-help" className="settings-field__help">
            0 posts the summary comment only
          </span>
          {capInvalid && (
            <span id="settings-inline-cap-error" role="alert" className="settings-field__error">
              Enter a whole number of 0 or more
            </span>
          )}
        </div>
        <div className="settings-field">
          <label className="settings-field__label" htmlFor="settings-min-severity">
            Minimum inline severity
          </label>
          <select
            id="settings-min-severity"
            className="settings-field__select"
            value={publishing.draft.publish_inline_min_severity}
            onChange={(e) => publishing.patch({ publish_inline_min_severity: e.target.value })}
            disabled={policyDisabled}
            aria-describedby="settings-min-severity-help"
          >
            {SEVERITIES.map((severity) => (
              <option key={severity} value={severity}>
                {severity}
              </option>
            ))}
          </select>
          <span id="settings-min-severity-help" className="settings-field__help">
            low will flood busy PRs
          </span>
        </div>
        <div className="settings-field settings-field--inline">
          <input
            id="settings-show-unverified"
            type="checkbox"
            checked={publishing.draft.publish_show_unverified}
            onChange={(e) => publishing.patch({ publish_show_unverified: e.target.checked })}
            disabled={policyDisabled}
          />
          <label className="settings-field__label" htmlFor="settings-show-unverified">
            Show unverified findings
          </label>
        </div>
      </SettingsSection>

      <SettingsSection
        title="Replies"
        description="What happens when an author answers an inline comment"
        dirty={replies.dirty}
        canSave={!repliesDisabled}
        saving={replies.saving}
        saved={replies.saved}
        error={replies.error}
        onSave={replies.save}
        onReset={replies.reset}
      >
        {noAuthorsSaved && (
          <p className="settings-section__notice">
            {noAuthorsDraft
              ? 'Reply modes are available once at least one author is saved in Publishing.'
              : 'Save the Publishing section above to enable reply modes.'}
          </p>
        )}
        <fieldset className="settings-field settings-field__modes" disabled={repliesDisabled}>
          <legend className="settings-field__label">Reply mode</legend>
          {REPLY_MODES.map(({ value, description }) => (
            <div key={value} className="settings-field__mode">
              <input
                id={`settings-reply-${value}`}
                type="radio"
                name="publish_reply_mode"
                value={value}
                checked={replies.draft.publish_reply_mode === value}
                onChange={() => chooseMode(value)}
                disabled={repliesDisabled}
                aria-describedby={`settings-reply-${value}-help`}
              />
              <label className="settings-field__mode-name" htmlFor={`settings-reply-${value}`}>
                {value}
              </label>
              <span id={`settings-reply-${value}-help`} className="settings-field__help">
                {description}
              </span>
            </div>
          ))}
        </fieldset>
        <p className="settings-field__help">{activeSince}</p>
        <p className="settings-field__help">
          Turning replies off and on again resets this cutoff; replies written in the gap are skipped
        </p>
        {replyTotals && (
          <p className="settings-field__totals">
            {[
              `${replyTotals.total} replies`,
              ...Object.entries(replyTotals.by_action).map(([action, count]) => `${action}: ${count}`),
              `${replyTotals.unlinked_roots ?? 0} unlinked roots`,
            ].join(', ')}
          </p>
        )}
      </SettingsSection>

      <SettingsSection
        title="Admins"
        description="Who can change these settings"
        dirty={admins.dirty}
        canSave={isAdmin}
        saving={admins.saving}
        saved={admins.saved}
        error={admins.error}
        onSave={admins.save}
        onReset={admins.reset}
      >
        <LoginListField
          id="settings-admins"
          label="Admins"
          value={admins.draft.admin_logins}
          onChange={changeAdmins}
          authors={false}
          disabled={disabled}
          fixed={settings.admin_logins_fixed}
          knownLogins={knownLogins}
        />
      </SettingsSection>
    </div>
  );
}
