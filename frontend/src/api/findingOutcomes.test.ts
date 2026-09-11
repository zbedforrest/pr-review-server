import { describe, expect, it, vi } from 'vitest';

const apiGetMock = vi.fn();
vi.mock('./client', () => ({
  apiGet: (path: string) => apiGetMock(path),
  apiPost: vi.fn(),
}));

import { fetchCriticalFindings } from './findingOutcomes';

describe('fetchCriticalFindings', () => {
  it('lists active critical claims and skips inactive records', async () => {
    apiGetMock.mockResolvedValue({
      commit_sha: 'abc',
      findings_available: true,
      findings: [
        { severity: 'critical', file: 'a.go', line: 3, comment: 'agent finding', state: 'confirmed', active: true },
        { severity: 'critical', file: 'a.go', line: 3, comment: 'first-pass duplicate', state: 'merged', active: false },
        { severity: 'medium', file: 'b.go', line: 1, comment: 'not critical', state: 'confirmed', active: true },
      ],
    });
    const { findings } = await fetchCriticalFindings('acme', 'example', 1);
    expect(findings.map(f => f.comment)).toEqual(['agent finding']);
  });

  it('treats findings without an active flag (schema 1) as active', async () => {
    apiGetMock.mockResolvedValue({
      commit_sha: 'abc',
      findings_available: true,
      findings: [{ severity: 'critical', file: 'a.go', line: 3, comment: 'old sidecar' }],
    });
    const { findings } = await fetchCriticalFindings('acme', 'example', 1);
    expect(findings).toHaveLength(1);
  });
});
