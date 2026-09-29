import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { ApprovalCandidates } from './ApprovalCandidates';
import * as api from '@/api/approval';
import { subscribeToWebSocketMessages } from '@/utils/websocket';
import type { ApprovalTarget } from '@/types/approval';

vi.mock('@/utils/websocket', () => ({ subscribeToWebSocketMessages: vi.fn(() => () => {}) }));
const dashboardPRs = vi.hoisted(() => ({ items: [{ owner: 'acme', repo: 'example', number: 1, commit_sha: 'a'.repeat(40), title: 'Retry handling', author: 'alex', via_teams: [], created_at: null }] }));
vi.mock('@/hooks/usePRs', () => ({ usePRs: () => ({ data: dashboardPRs.items }) }));
vi.mock('@/hooks/useCurrentUser', () => ({ useCurrentUser: () => ({ data: { github_username: 'sam' } }) }));
vi.mock('@/api/approval', async importOriginal => ({ ...(await importOriginal<typeof import('@/api/approval')>()), approvalRequest: vi.fn(), fetchApprovalCapabilities: vi.fn(), fetchApprovalTargets: vi.fn(), fetchApprovalScans: vi.fn(), fetchApprovalScan: vi.fn() }));
function mount() { const client = new QueryClient({ defaultOptions: { queries: { retry: false } } }); return { ...render(<QueryClientProvider client={client}><ApprovalCandidates filters={{}} /></QueryClientProvider>), client }; }
beforeEach(() => {
  vi.resetAllMocks();
  dashboardPRs.items = [{ ...dashboardPRs.items[0], number: 1 }];
  const slot = document.createElement('span'); slot.id = 'approval-action-slot'; document.body.append(slot);
  vi.mocked(api.fetchApprovalCapabilities).mockResolvedValue({ enabled: true, available: true, unavailable_reason: '', max_targets: 50 });
  vi.mocked(api.fetchApprovalScans).mockResolvedValue({ scans: [] });
  vi.mocked(api.fetchApprovalTargets).mockResolvedValue([]);
  vi.mocked(api.approvalRequest).mockResolvedValue({ scan_id: 'new-scan' });
  vi.mocked(api.fetchApprovalScan).mockResolvedValue({ scan: { scan_id: 'new-scan', status: 'queued', kind: 'full', cancel_requested: false, total: 1 }, targets: [] });
});
afterEach(() => { cleanup(); vi.restoreAllMocks(); document.getElementById('approval-action-slot')?.remove(); });
describe('approval investigation controls', () => {
  it('opening the launch panel does not start a model scan', async () => {
    mount(); fireEvent.click(await screen.findByText('Find approval candidates'));
    expect(api.approvalRequest).not.toHaveBeenCalled();
    fireEvent.click(screen.getByText('Investigate 1 PRs'));
    await waitFor(() => expect(api.approvalRequest).toHaveBeenCalledWith('approval-scans', expect.objectContaining({ targets: [{ owner: 'acme', repo: 'example', number: 1, expected_head_sha: 'a'.repeat(40) }] }), expect.any(String)));
  });
  it('keeps history visible but disables launch when runtime is unavailable', async () => {
    vi.mocked(api.fetchApprovalCapabilities).mockResolvedValue({ enabled: true, available: false, unavailable_reason: 'Native model unavailable', max_targets: 50 });
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

it('withdraws matching cached candidates immediately after review activity without a model request', async () => {
  const candidate = { target_id: 'target', scan_id: 'scan', owner: 'acme', repo: 'example', number: 1, revision: 'a'.repeat(40), execution_status: 'completed', decision: 'candidate', freshness_state: 'current', valid_until: new Date(Date.now() + 300000).toISOString(), reason_codes: [], summary: 'Supported' } as unknown as ApprovalTarget;
  vi.mocked(api.fetchApprovalTargets).mockResolvedValue([candidate]);
  const { client } = mount();
  await screen.findByText('View evidence');
  vi.mocked(api.fetchApprovalTargets).mockResolvedValue([{ ...candidate, freshness_state: 'stale' }]);
  const listener = vi.mocked(subscribeToWebSocketMessages).mock.calls[0][0];
  act(() => listener({ type: 'pr_updated', payload: { owner: 'acme', repo: 'example', number: 1 } }));
  expect(client.getQueryData<ApprovalTarget[]>(['approval-targets'])?.[0].freshness_state).toBe('stale');
  await waitFor(() => expect(screen.queryByText('View evidence')).toBeNull());
  expect(api.approvalRequest).not.toHaveBeenCalled();
});
it('does not subscribe to approval invalidation when disabled', async () => {
  vi.mocked(api.fetchApprovalCapabilities).mockResolvedValue({ enabled: false, available: false, unavailable_reason: 'Disabled', max_targets: 50 });
  const { client } = mount();
  await waitFor(() => expect(client.getQueryData(['approval-capabilities'])).toBeTruthy());
  expect(subscribeToWebSocketMessages).not.toHaveBeenCalled();
  expect(api.fetchApprovalTargets).not.toHaveBeenCalled();
});

it('does not retry failed validation after expiry changes, but permits a renewed validation cycle or reopening', async () => {
  const start = Date.now();
  const interval = vi.spyOn(window, 'setInterval');
  const expired = { target_id: 'target', scan_id: 'scan', owner: 'acme', repo: 'example', number: 1, revision: 'a'.repeat(40), execution_status: 'completed', decision: 'candidate', freshness_state: 'expired', valid_until: new Date(start - 1000).toISOString(), validated_at: new Date(start - 300000).toISOString(), reason_codes: [], summary: 'Historical rationale' } as unknown as ApprovalTarget;
  vi.mocked(api.fetchApprovalTargets).mockResolvedValue([expired]);
  vi.mocked(api.approvalRequest).mockResolvedValue({ states: { target: 'validating' } });
  const { client } = mount();
  await waitFor(() => expect(api.approvalRequest).toHaveBeenCalledTimes(1));
  const tick = interval.mock.calls.find(([, delay]) => delay === 1000)?.[0];
  expect(typeof tick).toBe('function');
  const clock = vi.spyOn(Date, 'now').mockReturnValue(start + 11000);
  const failed = { ...expired, valid_until: new Date(start + 1000).toISOString() };
  vi.mocked(api.fetchApprovalTargets).mockResolvedValue([failed]);
  await act(async () => {
    if (typeof tick === 'function') tick();
    client.setQueryData(['approval-targets'], [failed]);
  });
  expect(api.approvalRequest).toHaveBeenCalledTimes(1);
  const renewedThenExpired = { ...failed, validated_at: new Date(start + 2000).toISOString() };
  vi.mocked(api.fetchApprovalTargets).mockResolvedValue([renewedThenExpired]);
  clock.mockReturnValue(start + 22000);
  await act(async () => {
    if (typeof tick === 'function') tick();
    client.setQueryData(['approval-targets'], [renewedThenExpired]);
  });
  await waitFor(() => expect(api.approvalRequest).toHaveBeenCalledTimes(2));
  fireEvent.click(screen.getByText('Find approval candidates'));
  await waitFor(() => expect(api.approvalRequest).toHaveBeenCalledTimes(3));
});

function evidenceCandidate(): ApprovalTarget {
  return { target_id: 'target', scan_id: 'scan', owner: 'acme', repo: 'example', number: 1, revision: 'a'.repeat(40), execution_status: 'completed', decision: 'candidate', freshness_state: 'current', valid_until: new Date(Date.now() + 300000).toISOString(), validated_at: new Date().toISOString(), reason_codes: [], summary: 'Supported', assessment: { summary: 'Supported', sources: [], concerns: [], coverage_gaps: [], citations: [] } } as unknown as ApprovalTarget;
}
it('withdraws the row when fresh evidence reports a base change before the list refreshes', async () => {
  const candidate = evidenceCandidate();
  vi.mocked(api.fetchApprovalTargets).mockResolvedValue([candidate]);
  vi.mocked(api.approvalRequest).mockResolvedValue({ ...candidate, freshness_state: 'stale', reason_codes: ['base_changed'] });
  mount();
  fireEvent.click(await screen.findByText('View evidence'));
  await screen.findByText('base changed');
  expect(screen.queryByText('candidate')).toBeNull();
  expect(screen.queryByText('View evidence')).toBeNull();
});
it('keeps an initial evidence request running when an unrelated PR changes', async () => {
  const candidate = evidenceCandidate();
  vi.mocked(api.fetchApprovalTargets).mockResolvedValue([candidate]);
  let finish!: (target: ApprovalTarget) => void;
  vi.mocked(api.approvalRequest).mockImplementation(() => new Promise(resolve => { finish = resolve as typeof finish; }));
  mount();
  fireEvent.click(await screen.findByText('View evidence'));
  await waitFor(() => expect(api.approvalRequest).toHaveBeenCalledTimes(1));
  const listeners = vi.mocked(subscribeToWebSocketMessages).mock.calls;
  act(() => listeners[listeners.length - 1][0]({ type: 'pr_updated', payload: { owner: 'acme', repo: 'example', number: 2 } }));
  await act(async () => finish(candidate));
  await screen.findByText('candidate');
  expect(api.approvalRequest).toHaveBeenCalledTimes(1);
  expect(screen.queryByText('CancelledError')).toBeNull();
});
it('restarts a matching initial evidence request even before it has cached data', async () => {
  const candidate = evidenceCandidate();
  const stale = { ...candidate, freshness_state: 'stale', reason_codes: ['evidence_changed'] };
  vi.mocked(api.fetchApprovalTargets).mockResolvedValue([candidate]);
  vi.mocked(api.approvalRequest).mockImplementationOnce(() => new Promise(() => {})).mockResolvedValue(stale);
  mount();
  fireEvent.click(await screen.findByText('View evidence'));
  await waitFor(() => expect(api.approvalRequest).toHaveBeenCalledTimes(1));
  vi.mocked(api.fetchApprovalTargets).mockResolvedValue([stale]);
  const listeners = vi.mocked(subscribeToWebSocketMessages).mock.calls;
  act(() => listeners[listeners.length - 1][0]({ type: 'pr_updated', payload: { owner: 'acme', repo: 'example', number: 1 } }));
  await waitFor(() => expect(vi.mocked(api.approvalRequest).mock.calls.length).toBeGreaterThan(1));
  await screen.findByText('Checks and limitations');
  expect(screen.queryByText('CancelledError')).toBeNull();
});


it('uses the entire heading as an accessible disclosure without starting an investigation', async () => {
  mount();
  const heading = await screen.findByRole('heading', { name: 'Approval candidates (0)' });
  const toggle = screen.getByRole('button', { name: 'Approval candidates (0)' });
  expect(toggle.tagName).toBe('BUTTON');
  expect(toggle.getAttribute('type')).toBe('button');
  expect(heading.contains(toggle)).toBe(true);
  expect(toggle.querySelector('button')).toBeNull();
  expect(toggle.getAttribute('aria-expanded')).toBe('false');
  const controls = document.getElementById(toggle.getAttribute('aria-controls')!);
  expect(controls?.hidden).toBe(true);
  expect(screen.queryByRole('button', { name: 'Investigate' })).toBeNull();
  toggle.focus();
  expect(document.activeElement).toBe(toggle);
  const description = await screen.findByText('Find PRs where existing review evidence supports a quick human approval decision.');
  expect(toggle.contains(description)).toBe(true);
  expect(toggle.querySelector('svg')?.getAttribute('aria-hidden')).toBe('true');
  fireEvent.click(description);
  expect(toggle.getAttribute('aria-expanded')).toBe('true');
  expect(controls?.hidden).toBe(false);
  expect(screen.getByRole('button', { name: 'Investigate 1 PRs' })).toBeTruthy();
  expect(api.approvalRequest).not.toHaveBeenCalled();
  fireEvent.click(toggle);
  expect(toggle.getAttribute('aria-expanded')).toBe('false');
  expect(controls?.hidden).toBe(true);
  expect(screen.queryByRole('button', { name: 'Investigate 1 PRs' })).toBeNull();
  expect(api.approvalRequest).not.toHaveBeenCalled();
});


it('launches all 35 eligible PRs without sampling', async () => {
  dashboardPRs.items = Array.from({ length: 35 }, (_, index) => ({ ...dashboardPRs.items[0], number: index + 1 }));
  mount();
  fireEvent.click(await screen.findByRole('button', { name: 'Approval candidates (0)' }));
  const launch = screen.getByRole('button', { name: 'Investigate 35 PRs' }) as HTMLButtonElement;
  expect(launch.disabled).toBe(false);
  fireEvent.click(launch);
  await waitFor(() => expect(api.approvalRequest).toHaveBeenCalledWith('approval-scans', expect.objectContaining({ targets: dashboardPRs.items.map(pr => ({ owner: pr.owner, repo: pr.repo, number: pr.number, expected_head_sha: pr.commit_sha })) }), expect.any(String)));
});

it('requires narrowing filters above 50 eligible PRs', async () => {
  dashboardPRs.items = Array.from({ length: 51 }, (_, index) => ({ ...dashboardPRs.items[0], number: index + 1 }));
  mount();
  fireEvent.click(await screen.findByRole('button', { name: 'Approval candidates (0)' }));
  expect((screen.getByRole('button', { name: 'Investigate 51 PRs' }) as HTMLButtonElement).disabled).toBe(true);
  expect(screen.getByText('Narrow your filters to 50 PRs or fewer. No PRs will be sampled.')).toBeTruthy();
  expect(api.approvalRequest).not.toHaveBeenCalled();
});
