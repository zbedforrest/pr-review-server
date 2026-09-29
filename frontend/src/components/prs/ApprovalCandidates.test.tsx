import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { ApprovalCandidates } from './ApprovalCandidates';
import * as api from '@/api/approval';
import type { ApprovalTarget } from '@/types/approval';

vi.mock('@/hooks/usePRs', () => ({ usePRs: () => ({ data: [{ owner: 'acme', repo: 'example', number: 1, commit_sha: 'a'.repeat(40), title: 'Retry handling', author: 'alex', via_teams: [], created_at: null }] }) }));
vi.mock('@/hooks/useCurrentUser', () => ({ useCurrentUser: () => ({ data: { github_username: 'sam' } }) }));
vi.mock('@/api/approval', async importOriginal => ({ ...(await importOriginal<typeof import('@/api/approval')>()), approvalRequest: vi.fn(), fetchApprovalCapabilities: vi.fn(), fetchApprovalTargets: vi.fn(), fetchApprovalScans: vi.fn(), fetchApprovalScan: vi.fn() }));
function mount() { const client = new QueryClient({ defaultOptions: { queries: { retry: false } } }); return { ...render(<QueryClientProvider client={client}><ApprovalCandidates filters={{}} /></QueryClientProvider>), client }; }
beforeEach(() => {
  vi.resetAllMocks();
  const slot = document.createElement('span'); slot.id = 'approval-action-slot'; document.body.append(slot);
  vi.mocked(api.fetchApprovalCapabilities).mockResolvedValue({ enabled: true, available: true, unavailable_reason: '', max_targets: 25 });
  vi.mocked(api.fetchApprovalScans).mockResolvedValue({ scans: [] });
  vi.mocked(api.fetchApprovalTargets).mockResolvedValue([]);
  vi.mocked(api.approvalRequest).mockResolvedValue({ scan_id: 'new-scan' });
  vi.mocked(api.fetchApprovalScan).mockResolvedValue({ scan: { scan_id: 'new-scan', status: 'queued', kind: 'full', cancel_requested: false, total: 1 }, targets: [] });
});
afterEach(() => { cleanup(); document.getElementById('approval-action-slot')?.remove(); });
describe('approval investigation controls', () => {
  it('opening the launch panel does not start a model scan', async () => {
    mount(); fireEvent.click(await screen.findByText('Find approval candidates'));
    expect(api.approvalRequest).not.toHaveBeenCalled();
    fireEvent.click(screen.getByText('Investigate 1 PRs'));
    await waitFor(() => expect(api.approvalRequest).toHaveBeenCalledWith('approval-scans', expect.objectContaining({ targets: [{ owner: 'acme', repo: 'example', number: 1, expected_head_sha: 'a'.repeat(40) }] }), expect.any(String)));
  });
  it('keeps history visible but disables launch when runtime is unavailable', async () => {
    vi.mocked(api.fetchApprovalCapabilities).mockResolvedValue({ enabled: true, available: false, unavailable_reason: 'Native model unavailable', max_targets: 25 });
    mount(); fireEvent.click(await screen.findByText('Find approval candidates'));
    expect(screen.getByText('Native model unavailable')).toBeTruthy();
    expect((screen.getByText('Investigate 1 PRs') as HTMLButtonElement).disabled).toBe(true);
  });
  it('shows expired candidates in other results and explicitly revalidates only once', async () => {
    vi.mocked(api.fetchApprovalTargets).mockResolvedValue([{ target_id: 'target', scan_id: 'scan', owner: 'acme', repo: 'example', number: 1, execution_status: 'completed', decision: 'candidate', freshness_state: 'expired', valid_until: new Date(0).toISOString(), reason_codes: [], summary: 'Old decision' } as unknown as ApprovalTarget]);
    mount(); fireEvent.click(await screen.findByText('Find approval candidates'));
    await screen.findByText('Other results (1)');
    await waitFor(() => expect(api.approvalRequest).toHaveBeenCalledTimes(1));
    expect(api.approvalRequest).toHaveBeenCalledWith('approval-candidates/revalidate', { target_ids: ['target'] });
    expect(screen.queryByText('View evidence')).toBeNull();
  });
});

it('withdraws a cached open evidence decision when the current projection becomes stale', async () => {
  const candidate = { target_id: 'target', scan_id: 'scan', owner: 'acme', repo: 'example', number: 1, revision: 'a'.repeat(40), execution_status: 'completed', decision: 'candidate', freshness_state: 'current', valid_until: new Date(Date.now() + 300000).toISOString(), reason_codes: [], summary: 'Historical rationale', assessment: { sources: [], concerns: [], citations: [], coverage_gaps: [], summary: 'Historical rationale' } } as unknown as ApprovalTarget;
  vi.mocked(api.fetchApprovalTargets).mockResolvedValue([candidate]);
  vi.mocked(api.approvalRequest).mockResolvedValue(candidate);
  const { client } = mount();
  fireEvent.click(await screen.findByText('View evidence'));
  await screen.findByText('candidate');
  await act(async () => { client.setQueryData(['approval-targets'], [{ ...candidate, freshness_state: 'stale' }]); });
  await waitFor(() => expect(screen.queryByText('candidate')).toBeNull());
  expect(screen.queryByText('View evidence')).toBeNull();
  expect(screen.getAllByText('Historical rationale').length).toBeGreaterThan(0);
});
