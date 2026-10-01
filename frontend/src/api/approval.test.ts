import { afterEach, expect, it, vi } from 'vitest';
import { approvalRequest, fetchApprovalScans } from './approval';
afterEach(() => vi.unstubAllGlobals());
it('restores the latest full scan even after more than a page of rechecks', async () => {
  const fetchMock = vi.fn(async (url: string) => ({ ok: true, text: async () => JSON.stringify({ scans: url.includes('kind=full') ? [{ scan_id: 'full', kind: 'full' }] : Array.from({ length: 100 }, (_, index) => ({ scan_id: `recheck-${index}`, kind: 'recheck' })) }) }));
  vi.stubGlobal('fetch', fetchMock);
  const result = await fetchApprovalScans();
  expect(result.scans).toHaveLength(101);
  expect(result.scans.find(scan => scan.kind === 'full')?.scan_id).toBe('full');
  expect(fetchMock).toHaveBeenCalledTimes(2);
});

it('reports a plain-text rate limit as a readable error instead of a JSON parse failure', async () => {
  vi.stubGlobal('fetch', vi.fn(async () => ({ ok: false, status: 429, text: async () => 'Rate exceeded.' })));
  await expect(approvalRequest('approval-candidates')).rejects.toThrow('PRism is busy; retrying shortly');
  vi.unstubAllGlobals();
});
