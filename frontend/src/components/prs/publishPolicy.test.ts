import { describe, expect, it } from 'vitest';
import { OPTED_OUT_TITLE, PILOT_BLOCKED_TITLE, publishBlockedTitle, publishAllowedForAuthor } from './publishPolicy';

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

  it('matches a member of a listed team through the resolved members', () => {
    const teams = { core: { members: ['Bob', 'carol'], resolved_at: '2026-09-28T12:00:00Z' } };
    expect(publishAllowedForAuthor('bob', 'alice,team:core', teams)).toBe(true);
    expect(publishAllowedForAuthor('Carol', 'team:core', teams)).toBe(true);
    expect(publishAllowedForAuthor('bob', '@acme/core', teams)).toBe(true);
    expect(publishAllowedForAuthor('dave', 'alice,team:core', teams)).toBe(false);
    expect(publishAllowedForAuthor('core', 'team:core', teams)).toBe(false);
  });

  it('treats a team that is missing from the payload or unresolved as matching nobody', () => {
    expect(publishAllowedForAuthor('bob', 'team:core', undefined)).toBe(false);
    expect(publishAllowedForAuthor('bob', 'team:core', {})).toBe(false);
    expect(
      publishAllowedForAuthor('bob', 'team:core', {
        core: { members: ['bob'], resolved_at: '', error: 'team not found' },
      }),
    ).toBe(false);
  });

  it('never allows an empty author', () => {
    expect(publishAllowedForAuthor('', 'alice,')).toBe(false);
    expect(publishAllowedForAuthor('', '*')).toBe(false);
  });
});

describe('publishAllowedForAuthor with an opt-out list', () => {
  it('denies an opted-out login before any login, team or wildcard entry', () => {
    const teams = { core: { members: ['alice'], resolved_at: '2026-09-15T00:00:00Z' } };
    expect(publishAllowedForAuthor('alice', '*', undefined, 'Alice')).toBe(false);
    expect(publishAllowedForAuthor('alice', 'alice', undefined, ' alice ,bob')).toBe(false);
    expect(publishAllowedForAuthor('alice', 'team:core', teams, 'alice')).toBe(false);
    expect(publishAllowedForAuthor('bob', '*', undefined, 'alice')).toBe(true);
    expect(publishAllowedForAuthor('alice', '*', undefined, '')).toBe(true);
  });
});

describe('publishBlockedTitle', () => {
  it('names the opt-out when the author asked for silence', () => {
    expect(publishBlockedTitle('Alice', 'bob, alice')).toBe(OPTED_OUT_TITLE);
    expect(publishBlockedTitle('carol', 'bob, alice')).toBe(PILOT_BLOCKED_TITLE);
    expect(publishBlockedTitle('carol', undefined)).toBe(PILOT_BLOCKED_TITLE);
  });
});
