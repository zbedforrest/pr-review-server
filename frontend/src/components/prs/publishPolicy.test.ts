import { describe, expect, it } from 'vitest';
import { publishAllowedForAuthor } from './publishPolicy';

describe('publishAllowedForAuthor', () => {
  it('allows everyone when the list is the wildcard', () => {
    expect(publishAllowedForAuthor('alice', '*')).toBe(true);
    expect(publishAllowedForAuthor('anyone-at-all', ' * ')).toBe(true);
  });

  it('allows nobody when the list is empty or undefined', () => {
    expect(publishAllowedForAuthor('alice', undefined)).toBe(false);
    expect(publishAllowedForAuthor('alice', '')).toBe(false);
    expect(publishAllowedForAuthor('alice', '  ,  ')).toBe(false);
  });

  it('matches a listed login case-insensitively and ignores surrounding whitespace', () => {
    expect(publishAllowedForAuthor('Alice', 'bob, alice ,carol')).toBe(true);
    expect(publishAllowedForAuthor('alice', 'ALICE')).toBe(true);
    expect(publishAllowedForAuthor('dave', 'bob,alice,carol')).toBe(false);
  });

  it('requires a whole-login match, not a substring', () => {
    expect(publishAllowedForAuthor('ali', 'alice')).toBe(false);
    expect(publishAllowedForAuthor('alice', 'ali')).toBe(false);
  });

  it('never allows an empty author', () => {
    expect(publishAllowedForAuthor('', 'alice,')).toBe(false);
    expect(publishAllowedForAuthor('', '*')).toBe(false);
  });
});
