import { useCurrentUser } from '@/hooks/useCurrentUser';
import { usePublishOptOut } from '@/hooks/usePublishOptOut';
import './PublishOptOutBanner.scss';

// The anchor the footer link on every PRism comment points at.
export const PUBLISH_OPT_OUT_ANCHOR = 'prism-comments';

function enrolledVia(via: string | undefined): string {
  if (via === 'login') return 'by login';
  if (via === '*') return 'for everyone';
  if (via) return `via team ${via}`;
  return '';
}

export function PublishOptOutBanner() {
  const { data: user } = useCurrentUser();
  const { leave, rejoin } = usePublishOptOut();
  if (!user || (!user.publish_enrolled && !user.publish_opted_out)) return null;

  const optedOut = user.publish_opted_out === true;
  const pending = leave.isPending || rejoin.isPending;
  const error = leave.error ?? rejoin.error;

  return (
    <div id={PUBLISH_OPT_OUT_ANCHOR} className={`publish-opt-out-banner${optedOut ? ' publish-opt-out-banner--left' : ''}`} role="status">
      <span className="publish-opt-out-banner__text">
        {optedOut ? (
          <>
            <strong>PRism comments are off for your PRs.</strong> Nothing is posted on GitHub; your reviews still appear here.
          </>
        ) : (
          <>
            <strong>PRism comments on your PRs are on</strong> ({enrolledVia(user.enrolled_via)}). Leaving stops the comments; reviews stay on this dashboard.
          </>
        )}
        {error && <span className="publish-opt-out-banner__error"> {error.message}</span>}
      </span>
      <button
        type="button"
        className="publish-opt-out-banner__button"
        disabled={pending}
        onClick={() => (optedOut ? rejoin.mutate() : leave.mutate())}
      >
        {optedOut ? 'Rejoin' : 'Leave'}
      </button>
    </div>
  );
}
