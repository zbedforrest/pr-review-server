import { useMemo } from 'react';
import { ErrorMessage, LoadingSpinner } from '@/components/common';
import { useCurrentUser } from '@/hooks/useCurrentUser';
import { usePRs } from '@/hooks/usePRs';
import { usePublishReplies } from '@/hooks/usePublishReplies';
import { useSettingsEditor } from '@/hooks/useSettings';
import { SettingsForm } from './SettingsForm';
import '@/styles/main.scss';

export function SettingsPage() {
  const user = useCurrentUser();
  const settings = useSettingsEditor();
  const prs = usePRs();
  const isAdmin = user.data?.is_admin ?? false;
  const replies = usePublishReplies(isAdmin);

  const knownLogins = useMemo(() => prs.data && new Set(prs.data.map((pr) => pr.author.toLowerCase())), [prs.data]);

  const error = user.error ?? settings.error;

  return (
    <div className="app-container">
      <header className="app-header">
        <div className="app-header__branding">
          <h1>Settings</h1>
        </div>
        <div className="app-header__actions">
          <a href="/" className="app-header__action-link">
            Back to Dashboard
          </a>
        </div>
      </header>
      {error ? (
        <ErrorMessage message={error.message} />
      ) : !user.data || !settings.data ? (
        <LoadingSpinner />
      ) : (
        <SettingsForm
          settings={settings.data}
          isAdmin={user.data.is_admin}
          currentLogin={user.data.github_username}
          knownLogins={knownLogins}
          replyTotals={replies.data}
        />
      )}
    </div>
  );
}
