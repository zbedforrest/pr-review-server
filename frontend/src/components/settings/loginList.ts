const MAX_LOGIN_LEN = 39;
const BOT_SUFFIX = '[bot]';
const VALID_LOGIN = /^[a-z0-9](?:-?[a-z0-9]){0,38}$/;
const TEAM_PREFIX = 'team:';
const VALID_TEAM_SLUG = /^[a-z0-9](?:[a-z0-9._-]{0,98}[a-z0-9])?$/;

/**
 * The team slug named by an author-list entry: "team:<slug>" or "@<org>/<slug>"
 * (the server checks the org and stores the "team:" form). Null for a login.
 */
export function teamSlug(entry: string): string | null {
  const lower = entry.trim().toLowerCase();
  if (lower.startsWith(TEAM_PREFIX)) return lower.slice(TEAM_PREFIX.length);
  if (lower.startsWith('@')) {
    const slash = lower.indexOf('/');
    return slash > 1 ? lower.slice(slash + 1) : '';
  }
  return null;
}

/**
 * Mirrors the server's login rules: lowercase letters, digits and single
 * hyphens, no leading or trailing hyphen, at most 39 characters. A PR author
 * list also accepts "*", GitHub App authors such as "dependabot[bot]", and
 * team entries.
 */
export function isValidLogin(login: string, authors: boolean): boolean {
  if (authors && login === '*') return true;
  const slug = teamSlug(login);
  if (slug !== null) return authors && VALID_TEAM_SLUG.test(slug);
  const bot = login.endsWith(BOT_SUFFIX);
  if (bot && !authors) return false;
  const name = bot ? login.slice(0, -BOT_SUFFIX.length) : login;
  return name.length <= MAX_LOGIN_LEN && VALID_LOGIN.test(name);
}

export function normalizeLogins(raw: string, authors: boolean): { logins: string[]; invalid: string[] } {
  const logins: string[] = [];
  const invalid: string[] = [];
  const seen = new Set<string>();
  for (const part of raw.split(/[,\n]/)) {
    const login = part.trim().toLowerCase();
    if (login === '' || seen.has(login)) continue;
    seen.add(login);
    (isValidLogin(login, authors) ? logins : invalid).push(login);
  }
  return { logins, invalid };
}

export function joinLogins(logins: string[]): string {
  return logins.join(',');
}
