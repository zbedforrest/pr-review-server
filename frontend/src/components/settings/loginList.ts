const MAX_LOGIN_LEN = 39;
const BOT_SUFFIX = '[bot]';
const VALID_LOGIN = /^[a-z0-9](?:-?[a-z0-9]){0,38}$/;

/**
 * Mirrors the server's login rules: lowercase letters, digits and single
 * hyphens, no leading or trailing hyphen, at most 39 characters. A PR author
 * list also accepts "*" and GitHub App authors such as "dependabot[bot]".
 */
export function isValidLogin(login: string, authors: boolean): boolean {
  if (authors && login === '*') return true;
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
