/** Tooltip on any post-to-PR action that is disabled by the author gate. */
export const PILOT_BLOCKED_TITLE = 'Author is not in the comment pilot';

/**
 * Mirrors the server's publish_enabled_authors gate: a comma-separated list of
 * GitHub logins, or "*" for everyone. Empty or undefined means nobody, so the
 * dashboard never advertises "post to PR" for a PR the server will not post.
 */
export function publishAllowedForAuthor(author: string, enabledCsv: string | undefined): boolean {
  const login = author.trim().toLowerCase();
  if (!login || !enabledCsv) return false;
  const entries = enabledCsv
    .split(',')
    .map((s) => s.trim().toLowerCase())
    .filter(Boolean);
  return entries.includes('*') || entries.includes(login);
}
