import { useEffect, useRef, useState } from 'react';
import { generateReview } from '@/api/prs';
import { APIError } from '@/api/client';
import { parsePRRef } from '@/utils/parsePRRef';
import { useTelemetry } from '@/hooks/useTelemetry';

type Status =
  | { kind: 'idle' }
  | { kind: 'submitting' }
  | { kind: 'ok'; message: string }
  | { kind: 'error'; message: string };

const STATUS_RESET_MS = 6000;

/**
 * Header form: paste a GitHub PR URL (or owner/repo#N), Enter, and the server
 * starts an agentic review in the background — works for merged/closed PRs
 * the poller never ingested. The PR appears in the dashboard via the
 * generate-review broadcast; this form only reports submission.
 */
export function GenerateReviewForm() {
  const [value, setValue] = useState('');
  const [publish, setPublish] = useState(true);
  const [status, setStatus] = useState<Status>({ kind: 'idle' });
  const resetTimer = useRef<ReturnType<typeof setTimeout> | null>(null);
  const { track } = useTelemetry();

  useEffect(() => () => {
    if (resetTimer.current) clearTimeout(resetTimer.current);
  }, []);

  const showStatus = (next: Status) => {
    setStatus(next);
    if (resetTimer.current) clearTimeout(resetTimer.current);
    if (next.kind === 'ok' || next.kind === 'error') {
      resetTimer.current = setTimeout(() => setStatus({ kind: 'idle' }), STATUS_RESET_MS);
    }
  };

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault();
    const ref = parsePRRef(value);
    if (!ref) {
      showStatus({ kind: 'error', message: 'Not a PR URL — expected github.com/owner/repo/pull/N' });
      return;
    }
    track('generate_review_by_url', { pr_owner: ref.owner, pr_repo: ref.repo, pr_number: ref.number, publish });
    showStatus({ kind: 'submitting' });
    try {
      await generateReview({ ...ref, publish });
      setValue('');
      showStatus({ kind: 'ok', message: `Review started for ${ref.owner}/${ref.repo}#${ref.number}` });
    } catch (err) {
      if (err instanceof APIError && err.status === 409) {
        showStatus({ kind: 'ok', message: `Review already in progress for ${ref.owner}/${ref.repo}#${ref.number}` });
      } else if (err instanceof APIError && err.status === 404) {
        showStatus({ kind: 'error', message: `PR not found: ${ref.owner}/${ref.repo}#${ref.number}` });
      } else {
        showStatus({ kind: 'error', message: 'Failed to start review — see server logs' });
      }
    }
  };

  return (
    <form className="generate-review" onSubmit={handleSubmit}>
      <input
        type="text"
        className="generate-review__input"
        placeholder="Paste a PR URL to review…"
        aria-label="PR URL to review"
        value={value}
        onChange={(e) => setValue(e.target.value)}
        disabled={status.kind === 'submitting'}
        spellCheck={false}
      />
      <label className="generate-review__publish">
        <input
          type="checkbox"
          checked={publish}
          onChange={(e) => setPublish(e.target.checked)}
          disabled={status.kind === 'submitting'}
        />
        Post to PR as the Prism bot
      </label>
      <button
        type="submit"
        className="app-header__action-btn"
        disabled={status.kind === 'submitting' || value.trim() === ''}
      >
        {status.kind === 'submitting' ? 'Starting…' : 'Review'}
      </button>
      {(status.kind === 'ok' || status.kind === 'error') && (
        <span
          role="status"
          className={`generate-review__status generate-review__status--${status.kind}`}
        >
          {status.message}
        </span>
      )}
    </form>
  );
}
