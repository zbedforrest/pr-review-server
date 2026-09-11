import { describe, expect, it } from 'vitest';
import { CONFIDENCE_BADGES } from './confidenceBadges';

describe('CONFIDENCE_BADGES', () => {
  it('has one medal per score, ascending from 0 to 5', () => {
    expect(CONFIDENCE_BADGES.map((b) => b.score)).toEqual([0, 1, 2, 3, 4, 5]);
  });

  it('gives every medal a distinct name, rank word and tagline', () => {
    for (const key of ['name', 'rankWord', 'tagline'] as const) {
      const values = CONFIDENCE_BADGES.map((b) => b[key]);
      expect(new Set(values).size).toBe(values.length);
      expect(values.every((v) => v.trim().length > 0)).toBe(true);
    }
  });

  it('ships inline SVGs on the shared 64x64 canvas', () => {
    for (const { svg } of CONFIDENCE_BADGES) {
      expect(svg.startsWith('<svg')).toBe(true);
      expect(svg.endsWith('</svg>')).toBe(true);
      expect(svg).toContain("viewBox='0 0 64 64'");
    }
  });

  it('keeps the SVGs free of scripts, text, images and fixed dimensions', () => {
    for (const { svg } of CONFIDENCE_BADGES) {
      expect(svg).not.toMatch(/<script/i);
      expect(svg).not.toMatch(/<text/i);
      expect(svg).not.toMatch(/<image/i);
      expect(svg).not.toMatch(/\swidth=/i);
      expect(svg).not.toMatch(/\sheight=/i);
    }
  });

  it('defines every gradient a fill references, namespaced by score', () => {
    for (const { score, svg } of CONFIDENCE_BADGES) {
      const defined = new Set([...svg.matchAll(/\sid='([^']+)'/g)].map((m) => m[1]));
      const referenced = [...svg.matchAll(/url\(#([^)]+)\)/g)].map((m) => m[1]);
      expect(referenced.length).toBeGreaterThan(0);
      for (const id of referenced) {
        expect(defined.has(id), `${id} referenced but not defined`).toBe(true);
      }
      for (const id of defined) {
        expect(id.startsWith(`g${score}-`), `${id} not namespaced for score ${score}`).toBe(true);
      }
    }
  });
});
