import { afterEach, expect, it, vi } from 'vitest';
import { fetchApprovalScans } from './approval';
afterEach(() => vi.unstubAllGlobals());
it('restores the latest full scan even after more than a page of rechecks', async () => {
  const fetchMock = vi.fn(async (url: string) => ({ ok: true, json: async () => ({ scans: url.includes('kind=full') ? [{ scan_id: 'full', kind: 'full' }] : Array.from({ length: 100 }, (_, index) => ({ scan_id: `recheck-${index}`, kind: 'recheck' })) }) }));
  vi.stubGlobal('fetch', fetchMock);
  const result = await fetchApprovalScans();
  expect(result.scans).toHaveLength(101);
  expect(result.scans.find(scan => scan.kind === 'full')?.scan_id).toBe('full');
  expect(fetchMock).toHaveBeenCalledTimes(2);
});
