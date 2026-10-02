import type { AuthorListTeam } from '@/api/settings';
import { teamSlug } from '@/components/settings/loginList';

/** Tooltip on any post-to-PR action that is disabled by the author gate. */
export const PILOT_BLOCKED_TITLE = 'Author is not in the comment pilot';
export const OPTED_OUT_TITLE = 'Author opted out of PRism comments';

/** Names why the gate denies the author, for the disabled post controls. */
export function publishBlockedTitle(author: string, optOutCsv: string | undefined): string {
  return entriesOf(optOutCsv).includes(author.trim().toLowerCase()) ? OPTED_OUT_TITLE : PILOT_BLOCKED_TITLE;
}

const entriesOf = (csv: string | undefined): string[] =>
  (csv ?? '')
    .split(',')
    .map((s) => s.trim().toLowerCase())
    .filter(Boolean);

/**
 * Mirrors the server's publish gate: publish_enabled_authors is a
 * comma-separated list of GitHub logins, "team:<slug>" entries, or "*" for
 * everyone, and publish_opt_out_authors lists the logins that asked for
 * silence, which deny before any entry can match. Empty or undefined lists
 * mean nobody, so the dashboard never advertises "post to PR" for a PR the
 * server will not post. Team entries match through the members the server
 * resolved and returned alongside the settings; an unresolved team matches
 * nobody, as on the server.
 */
export function publishAllowedForAuthor(
  author: string,
  enabledCsv: string | undefined,
  teams: Record<string, AuthorListTeam> | undefined = undefined,
  optOutCsv: string | undefined = undefined,
): boolean {
  const login = author.trim().toLowerCase();
  if (!login || !enabledCsv) return false;
  if (entriesOf(optOutCsv).includes(login)) return false;
  const entries = entriesOf(enabledCsv);
  if (entries.includes('*') || entries.includes(login)) return true;
  return entries.some((entry) => {
    const slug = teamSlug(entry);
    if (slug === null) return false;
    const team = teams?.[slug];
    return team !== undefined && !team.error && team.members.some((m) => m.toLowerCase() === login);
  });
}
