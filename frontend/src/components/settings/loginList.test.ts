import { describe, expect, it } from 'vitest';
import { isValidLogin, joinLogins, normalizeLogins } from './loginList';

const login39 = 'a'.repeat(39);
const login40 = 'a'.repeat(40);

describe('isValidLogin', () => {
  it.each([
    ['alice', true],
    ['a', true],
    ['alice-1', true],
    ['a-b-c', true],
    ['0', true],
    [login39, true],
    [login40, false],
    ['-a', false],
    ['alice-', false],
    ['a--b', false],
    ['Alice', false],
    ['al ice', false],
    ['alice_1', false],
    ['', false],
    ['-', false],
    ['[bot]', false],
  ])('%j is valid for admins: %s', (login, expected) => {
    expect(isValidLogin(login, false)).toBe(expected);
    expect(isValidLogin(login, true)).toBe(expected);
  });

  it('accepts "*" only for authors', () => {
    expect(isValidLogin('*', true)).toBe(true);
    expect(isValidLogin('*', false)).toBe(false);
  });

  it('accepts a [bot] suffix only for authors, with the same rules on the name', () => {
    expect(isValidLogin('dependabot[bot]', true)).toBe(true);
    expect(isValidLogin('dependabot[bot]', false)).toBe(false);
    expect(isValidLogin(`${login39}[bot]`, true)).toBe(true);
    expect(isValidLogin(`${login40}[bot]`, true)).toBe(false);
    expect(isValidLogin('-dependabot[bot]', true)).toBe(false);
    expect(isValidLogin('a[bot][bot]', true)).toBe(false);
  });
});

describe('normalizeLogins', () => {
  it('splits on commas and newlines, trims, lowercases and drops blanks', () => {
    expect(normalizeLogins(' Alice ,bob\ncarol\r\n\n , ,dave', false)).toEqual({
      logins: ['alice', 'bob', 'carol', 'dave'],
      invalid: [],
    });
  });

  it('dedupes case-insensitively keeping first-seen order', () => {
    expect(normalizeLogins('bob,Alice,alice,BOB,carol', false).logins).toEqual(['bob', 'alice', 'carol']);
  });

  it('reports invalid entries separately, lowercased, without dropping the valid ones', () => {
    expect(normalizeLogins('alice,Al ice,-bob,carol', false)).toEqual({
      logins: ['alice', 'carol'],
      invalid: ['al ice', '-bob'],
    });
  });

  it('treats "*" and [bot] authors as invalid unless authors is true', () => {
    expect(normalizeLogins('*,dependabot[bot]', false).invalid).toEqual(['*', 'dependabot[bot]']);
    expect(normalizeLogins('*,dependabot[bot]', true)).toEqual({
      logins: ['*', 'dependabot[bot]'],
      invalid: [],
    });
  });

  it('returns empty lists for blank input', () => {
    expect(normalizeLogins('', false)).toEqual({ logins: [], invalid: [] });
    expect(normalizeLogins(' \n , ', true)).toEqual({ logins: [], invalid: [] });
  });
});

describe('joinLogins', () => {
  it('joins with bare commas, the shape the server stores', () => {
    expect(joinLogins(['alice', 'bob'])).toBe('alice,bob');
    expect(joinLogins([])).toBe('');
  });
});
