import { useEffect, useMemo, useRef, useState } from 'react';
import { createPortal } from 'react-dom';
import { isCancelledError, useQuery, useQueryClient, type Query } from '@tanstack/react-query';
import { ApprovalAPIError, approvalRequest, fetchApprovalCapabilities, fetchApprovalProgress, fetchApprovalScan, fetchApprovalScans, fetchApprovalTargets, targetPath } from '@/api/approval';
import { usePRs } from '@/hooks/usePRs';
import { useCurrentUser } from '@/hooks/useCurrentUser';
import { approvalBucket, approvalKey, approvalScope, isApprovalCandidate, reconcileApprovalTarget, safeEvidenceURL } from '@/utils/approval';
import { subscribeToWebSocketMessages } from '@/utils/websocket';
import type { PR } from '@/types/pr';
import { filterAndSortPRs, type PRFilterCriteria } from '@/utils/sectionFilters';
import type { ApprovalCitation, ApprovalConcernScore, ApprovalScan, ApprovalScore, ApprovalTarget } from '@/types/approval';
import '@/styles/components/_approval-candidates.scss';
import { ApprovalPrism } from './ApprovalPrism';

const activeStates = ['queued', 'running', 'cancelling'];
const terminalStates = ['completed', 'failed', 'timed_out', 'cancelled'];
const label = (value: string) => value.replace(/_/g, ' ');
const time = (value?: string | null) => value ? new Date(value).toLocaleString() : 'Not yet validated';

function Citation({ citation, target }: { citation: ApprovalCitation; target: ApprovalTarget }) {
  const evidence = target.snapshot?.evidence?.find(item => item.id === citation.evidence_id);
  const codeURL = citation.path && citation.revision && /^[a-f0-9]{40}$/i.test(citation.revision)
    ? `https://github.com/${encodeURIComponent(target.owner)}/${encodeURIComponent(target.repo)}/blob/${citation.revision}/${citation.path.split('/').map(encodeURIComponent).join('/')}#L${citation.start_line}-L${citation.end_line}` : undefined;
  const href = safeEvidenceURL(evidence?.url || codeURL || '');
  return <div className="approval-citation">{href ? <a href={href} target="_blank" rel="noreferrer">{citation.path ? `${citation.path}:${citation.start_line}` : 'Review evidence'}</a> : <span>Evidence reference</span>}<blockquote>{citation.excerpt}</blockquote></div>;
}

export function ApprovalCandidates({ filters }: { filters: PRFilterCriteria }) {
  const client = useQueryClient();
  const { data: prs = [] } = usePRs();
  const { data: user } = useCurrentUser();
  const capabilities = useQuery({ queryKey: ['approval-capabilities'], queryFn: fetchApprovalCapabilities, staleTime: 60000, refetchInterval: 60000 });
  const enabled = capabilities.data?.enabled === true;
  const [validationPollingUntil, setValidationPollingUntil] = useState(0);
  const scans = useQuery({ queryKey: ['approval-scans'], queryFn: fetchApprovalScans, enabled, refetchInterval: query => query.state.error ? 30000 : query.state.data?.scans?.some(scan => activeStates.includes(scan.status)) ? 2000 : 60000 });
  const targets = useQuery({ queryKey: ['approval-targets'], queryFn: fetchApprovalTargets, enabled, refetchInterval: query => query.state.error ? 30000 : scans.data?.scans?.some(scan => activeStates.includes(scan.status)) ? 10000 : Date.now() < validationPollingUntil ? 2000 : 60000 });
  const [selectedScan, setSelectedScan] = useState<string>();
  const [selected, setSelected] = useState<ApprovalTarget>();
  const [open, setOpen] = useState(false);
  const [view, setView] = useState<'all' | 'candidate' | 'close' | 'blocked' | 'work'>('all');
  const [busy, setBusy] = useState(false);
  // While a scan request is admitting (the server checks every PR first), show
  // the prism at once instead of waiting for the scan to exist.
  const [launching, setLaunching] = useState(0);
  const [error, setError] = useState('');
  const [now, setNow] = useState(Date.now());
  const [slot, setSlot] = useState<HTMLElement | null>(null);
  const sectionRef = useRef<HTMLElement>(null);
  const evidenceRef = useRef<HTMLElement>(null);
  const returnFocus = useRef<HTMLElement | null>(null);
  const focusedEvidence = useRef<string>();
  const validated = useRef(new Set<string>());
  const validationAttempts = useRef(new Map<string, number>());
  const lastValidationRequest = useRef(0);
  const scanList = scans.data?.scans || [];
  const active = scanList.find(scan => activeStates.includes(scan.status));
  const currentScanID = active?.scan_id || selectedScan || scanList.find(scan => scan.kind === 'full')?.scan_id;
  const scan = useQuery({ queryKey: ['approval-scan', currentScanID], queryFn: () => fetchApprovalScan(currentScanID!), enabled: enabled && !!currentScanID, refetchInterval: query => query.state.error ? 30000 : activeStates.includes(query.state.data?.scan.status || '') ? 10000 : false });
  const progress = useQuery({ queryKey: ['approval-progress', currentScanID], queryFn: () => fetchApprovalProgress(currentScanID!), enabled: enabled && !!currentScanID, retry: false, refetchInterval: query => query.state.error ? active ? 10000 : false : activeStates.includes(query.state.data?.status || 'running') ? 2000 : false });
  const detail = useQuery({ queryKey: ['approval-evidence', selected?.target_id], queryFn: () => approvalRequest<ApprovalTarget>(targetPath(selected!)), enabled: enabled && !!selected });
  const scope = useMemo(() => approvalScope(prs, filters, user?.github_username), [prs, filters, user?.github_username]);
  const visiblePRs = useMemo(() => filterAndSortPRs(prs, filters, user?.github_username), [prs, filters, user?.github_username]);
  const visibleKeys = useMemo(() => new Set(visiblePRs.filter(pr => !pr.hidden).map(approvalKey)), [visiblePRs]);
  const prByKey = new Map(prs.map(pr => [approvalKey(pr), pr]));
  const allTargets = (targets.data || []).map(target => { const pr = prByKey.get(approvalKey(target)); return pr ? reconcileApprovalTarget(target, pr) : target; });
  const ordered = new Map(visiblePRs.map((pr, index) => [approvalKey(pr), index]));
  const visible = allTargets.filter(target => visibleKeys.has(approvalKey(target))).sort((a, b) => (b.score?.value ?? -1) - (a.score?.value ?? -1) || (ordered.get(approvalKey(a)) || 0) - (ordered.get(approvalKey(b)) || 0));
  const candidates = visible.filter(target => isApprovalCandidate(target, now));
  const completed = progress.data?.finished ?? (scan.data?.targets || []).filter(target => terminalStates.includes(target.execution_status)).length;
  const runningTargets = (scan.data?.targets || []).filter(target => ['collecting', 'investigating', 'validating'].includes(target.execution_status));
  const activitySummary = active?.cancel_requested ? 'Stopping investigators and cancelling remaining pull requests' : progress.data?.summary || (runningTargets.some(target => target.execution_status === 'validating') ? 'Rechecking current code and review evidence' : runningTargets.some(target => target.execution_status === 'investigating') ? 'Investigating existing reviews and supporting code' : runningTargets.length ? 'Gathering existing reviews and pull request evidence' : 'Waiting for an available investigator');
  const scanCandidates = (scan.data?.targets || []).filter(target => { const pr = prByKey.get(approvalKey(target)); const latest = allTargets.find(item => approvalKey(item) === approvalKey(target)); return pr && latest && isApprovalCandidate(reconcileApprovalTarget(target, pr, latest), now); }).length;
  const overLimit = scope.length > (capabilities.data?.max_targets || 50);
  const selectedPR = selected ? prByKey.get(approvalKey(selected)) : undefined;
  const latestSelected = selected ? allTargets.find(target => approvalKey(target) === approvalKey(selected)) : undefined;
  const currentDetail = detail.data && selectedPR && !selectedPR.hidden && latestSelected
    ? reconcileApprovalTarget(detail.data, selectedPR, latestSelected) : undefined;

  useEffect(() => {
    if (!progress.data || activeStates.includes(progress.data.status)) return;
    void client.invalidateQueries({ queryKey: ['approval-scans'] });
    void client.invalidateQueries({ queryKey: ['approval-targets'] });
    void client.invalidateQueries({ queryKey: ['approval-scan', progress.data.scan_id] });
  }, [client, progress.data]);
  useEffect(() => {
    if (!enabled) return;
    return subscribeToWebSocketMessages(message => {
      if (!['pr_updated', 'pr_deleted'].includes(message.type) || !message.payload) return;
      const pr = message.payload as Partial<PR>;
      if (!pr.owner || !pr.repo || typeof pr.number !== 'number') return;
      const key = approvalKey({ owner: pr.owner, repo: pr.repo, number: pr.number });
      const withdraw = (target: ApprovalTarget): ApprovalTarget => approvalKey(target) === key
        ? { ...target, freshness_state: 'stale', reason_codes: Array.from(new Set([...(target.reason_codes || []), 'evidence_changed'])) } : target;
      const affectedIDs = new Set((client.getQueryData<ApprovalTarget[]>(['approval-targets']) || []).filter(target => approvalKey(target) === key).map(target => target.target_id));
      if (selected && approvalKey(selected) === key) affectedIDs.add(selected.target_id);
      const matchesEvidence = (query: Query) => query.queryKey[0] === 'approval-evidence' &&
        (affectedIDs.has(String(query.queryKey[1])) || (!!query.state.data && approvalKey(query.state.data as ApprovalTarget) === key));
      void client.cancelQueries({ queryKey: ['approval-targets'] }, { revert: false });
      void client.cancelQueries({ predicate: matchesEvidence }, { revert: false });
      // Updates refetch instead: the server stales results only for material changes, before broadcasting.
      if (message.type === 'pr_deleted') {
        client.setQueryData<ApprovalTarget[]>(['approval-targets'], old => old?.map(withdraw));
        client.setQueriesData<ApprovalTarget>({ queryKey: ['approval-evidence'] }, old => old ? withdraw(old) : old);
        client.setQueriesData<{ scan: ApprovalScan; targets: ApprovalTarget[] | null }>({ queryKey: ['approval-scan'] }, old => old ? { ...old, targets: old.targets?.map(withdraw) || null } : old);
      }
      void client.invalidateQueries({ queryKey: ['approval-targets'] });
      void client.invalidateQueries({ predicate: matchesEvidence });
    });
  }, [client, enabled, selected]);
  useEffect(() => { setSlot(document.getElementById('approval-action-slot')); const interval = window.setInterval(() => setNow(Date.now()), 1000); return () => window.clearInterval(interval); }, []);
  useEffect(() => {
    if (!enabled || !targets.data || now - lastValidationRequest.current < 10000) return;
    const pending = targets.data.filter(target => visibleKeys.has(approvalKey(target)) && target.decision === 'candidate' && target.execution_status === 'completed' && target.freshness_state !== 'stale' && (open || !isApprovalCandidate(target, now)) && (!validated.current.has(target.target_id) || (!isApprovalCandidate(target, now) && !validated.current.has(`${target.target_id}:${target.validated_at || "never"}`)))).slice(0, 2);
    if (!pending.length) return;
    lastValidationRequest.current = now;
    pending.forEach(target => { validated.current.add(target.target_id); validated.current.add(`${target.target_id}:${target.validated_at || "never"}`); });
    void approvalRequest<{ states?: Record<string, string> }>('approval-candidates/revalidate', { target_ids: pending.map(target => target.target_id) }).then(response => {
      if (Object.values(response?.states || {}).some(state => state === 'validating' || state === 'busy_or_already_running')) setValidationPollingUntil(Date.now() + 30000);
      pending.forEach(target => {
        const key = `${target.target_id}:${target.validated_at || "never"}`;
        const attempts = (validationAttempts.current.get(key) || 0) + 1;
        validationAttempts.current.set(key, attempts);
        if (response?.states?.[target.target_id] === 'busy_or_already_running' && attempts < 3) { validated.current.delete(target.target_id); validated.current.delete(key); }
      });
      return client.invalidateQueries({ queryKey: ['approval-targets'] });
    }).catch((err: Error) => setError(err.message));
  }, [enabled, open, targets.data, now, visibleKeys, client]);
  useEffect(() => {
    if (detail.data?.freshness_state !== 'stale') return;
    const stale = detail.data;
    client.setQueryData<ApprovalTarget[]>(['approval-targets'], old => old?.map(target => target.target_id === stale.target_id && target.freshness_state !== 'stale'
      ? { ...target, freshness_state: 'stale', reason_codes: Array.from(new Set([...(target.reason_codes || []), ...(stale.reason_codes || [])])) } : target));
  }, [client, detail.data]);
  useEffect(() => {
    if (selected && currentDetail && focusedEvidence.current !== selected.target_id) { evidenceRef.current?.focus(); focusedEvidence.current = selected.target_id; }
  }, [selected, currentDetail]);
  useEffect(() => {
    if (selected?.target_id) void client.invalidateQueries({ queryKey: ['approval-evidence', selected.target_id] });
  }, [client, selected?.target_id, latestSelected?.target_id, latestSelected?.freshness_state, latestSelected?.valid_until, selectedPR?.commit_sha, selectedPR?.draft, selectedPR?.hidden]);

  const refresh = async () => { await Promise.all([client.invalidateQueries({ queryKey: ['approval-scans'] }), client.invalidateQueries({ queryKey: ['approval-targets'] }), client.invalidateQueries({ queryKey: ['approval-scan'] })]); };
  const perform = async (operation: () => Promise<unknown>) => {
    setBusy(true); setError('');
    try { await operation(); await refresh(); } catch (err) {
      setError(err instanceof Error ? err.message : 'Investigation request failed');
      if (err instanceof ApprovalAPIError && err.scanID) setSelectedScan(err.scanID);
    } finally { setBusy(false); setLaunching(0); }
  };
  const launch = () => perform(async () => {
    setLaunching(scope.length);
    const result = await approvalRequest<ApprovalScan>('approval-scans', {
      targets: scope.map(pr => ({ owner: pr.owner, repo: pr.repo, number: pr.number, expected_head_sha: pr.commit_sha })),
      scope: { repositories: filters.repos || [], teams: filters.teams || [], states: filters.states || [], search: filters.search || '' },
    }, crypto.randomUUID()); setSelectedScan(result.scan_id);
  });
  const closeEvidence = () => { focusedEvidence.current = undefined; setSelected(undefined); returnFocus.current?.focus(); };
  const showEvidence = (target: ApprovalTarget, element: HTMLElement) => { returnFocus.current = element; setSelected(target); };
  const toggle = () => { setOpen(value => { if (!value) { validated.current.clear(); validationAttempts.current.clear(); lastValidationRequest.current = 0; } return !value; }); sectionRef.current?.scrollIntoView?.({ block: 'nearest', behavior: 'smooth' }); };
  const recheck = (target: ApprovalTarget) => perform(async () => { const result = await approvalRequest<ApprovalScan>(`${targetPath(target)}/recheck`, { expected_head_sha: prs.find(pr => approvalKey(pr) === approvalKey(target))?.commit_sha || target.revision }, crypto.randomUUID()); setSelectedScan(result.scan_id); setSelected(undefined); });
  const title = (target: ApprovalTarget) => prs.find(pr => approvalKey(pr) === approvalKey(target))?.title || `${target.owner}/${target.repo}`;

  if (!enabled) return null;
  const shownError = [error, targets.error, scans.error].map(err => err instanceof Error ? (isCancelledError(err) ? '' : err.message) : err || '').find(Boolean);
  const rows = visible.map(target => ({ target, status: rowStatus(target, now) }));
  const counts = { all: rows.length, candidate: rows.filter(r => r.status.kind === 'candidate').length, close: rows.filter(r => r.status.kind === 'close').length, blocked: rows.filter(r => r.status.kind === 'blocked').length, work: rows.filter(r => r.status.kind === 'work' || r.status.kind === 'other').length };
  const shownRows = rows.filter(r => view === 'all' || (view === 'work' ? r.status.kind === 'work' || r.status.kind === 'other' : r.status.kind === view));
  const lastScan = scan.data?.scan;
  return <>
    {slot && createPortal(<button className="app-header__action-btn" onClick={toggle} aria-expanded={open} aria-controls="approval-candidate-controls">{active ? 'View investigation' : 'Find approval candidates'}</button>, slot)}
    <section id="approval-candidates" ref={sectionRef} className={`approval-candidates${active ? ' approval-candidates--running' : ''}`} aria-label="Approval candidates">
      <h2 className="approval-disclosure-heading" aria-labelledby="approval-disclosure-title"><button type="button" className="approval-disclosure" aria-labelledby="approval-disclosure-title" onClick={toggle} aria-expanded={open} aria-controls="approval-candidate-controls"><span className="approval-disclosure-row"><span id="approval-disclosure-title">Approval candidates <span className="approval-disclosure-count">({candidates.length})</span></span><svg className="approval-disclosure-chevron" aria-hidden="true" viewBox="0 0 20 20" width="20" height="20"><path d="m6 8 4 4 4-4" fill="none" stroke="currentColor" strokeWidth="1.75" strokeLinecap="round" strokeLinejoin="round" /></svg></span>{!open && !allTargets.length && !active && <span className="approval-disclosure-description">{targets.isPending ? 'Loading investigations...' : 'Find PRs where existing review evidence supports a quick human approval decision.'}</span>}</button></h2>
      {active && <ApprovalPrism finished={progress.data?.finished ?? completed} total={progress.data?.total ?? active.total} summary={activitySummary} running={progress.data?.running ?? runningTargets.length} reconnecting={!!progress.error && !!scan.error} />}
      {!active && launching > 0 && <ApprovalPrism finished={0} total={launching} summary={`Checking ${launching} pull requests before investigating`} running={0} reconnecting={false} />}
      <div id="approval-candidate-controls" className="approval-toolbar" hidden={!open}>
        <div className="approval-toolbar-text">
          <span>Score PRs in your current filters by how ready they are for a human approval.</span>
          <span className="approval-muted">{scope.length} PRs · {filters.repos?.join(', ') || 'All repositories'} · {filters.teams?.join(', ') || 'All teams'}{filters.search ? ` · “${filters.search}”` : ''}</span>
        </div>
        <button className="approval-primary" onClick={launch} disabled={busy || !!active || !capabilities.data?.available || overLimit || !scope.length}>{launching ? 'Starting investigation...' : `Investigate ${scope.length} PRs`}</button>
        {!capabilities.data?.available && <p role="status" className="approval-notice">{capabilities.data?.unavailable_reason}</p>}
        {overLimit && <p role="status" className="approval-notice">Narrow your filters to {capabilities.data?.max_targets} PRs or fewer. No PRs will be sampled.</p>}
        {!scope.length && <p className="approval-notice">No eligible PRs in these filters. Your own, hidden, closed and already approved current revisions are excluded.</p>}
      </div>
      {shownError && <p role="alert" className="approval-notice approval-notice--error">{shownError}</p>}
      {lastScan && <div className="approval-scanline" role="status" aria-live="polite"><span>{lastScan.kind === 'recheck' ? 'Recheck' : 'Last scan'} {label(lastScan.status)} · {completed} of {lastScan.total} finished · {scanCandidates} candidates</span>{active && <button disabled={busy || active.cancel_requested} onClick={() => perform(() => approvalRequest(`approval-scans/${active.scan_id}/cancel`, {}))}>{active.cancel_requested ? 'Cancelling remaining' : 'Cancel remaining'}</button>}</div>}
      {open && !allTargets.length && !active && <p className="approval-muted">{targets.isPending ? 'Loading investigations...' : 'Find PRs where existing review evidence supports a quick human approval decision.'}</p>}
      {!rows.length && allTargets.length > visible.length && <p className="approval-muted">{allTargets.length - visible.length} results hidden by filters</p>}
      {!!rows.length && <div className={`approval-layout${selected ? ' approval-layout--selected' : ''}`}>
        <div className="approval-list">
          <div className="approval-filters" role="group" aria-label="Filter results">
            {([['all', 'All'], ['candidate', 'Candidates'], ['close', 'Close'], ['blocked', 'Blocked'], ['work', 'Needs work']] as const).map(([key, text]) => <button key={key} aria-pressed={view === key} className={view === key ? 'is-active' : ''} onClick={() => setView(key)}>{text} <span>{counts[key]}</span></button>)}
            {allTargets.length > visible.length && <span className="approval-muted">{allTargets.length - visible.length} hidden by filters</span>}
          </div>
          {!shownRows.length && <p className="approval-muted">Nothing in this view.</p>}
          {!!shownRows.length && <table className="approval-table"><caption className="approval-sr-only">Approval results sorted by score</caption>
            <colgroup><col className="approval-col-score" /><col /><col className="approval-col-status" /><col className="approval-col-action" /></colgroup>
            <thead><tr><th scope="col">Score</th><th scope="col">Pull request</th><th scope="col">Status</th><th scope="col"><span className="approval-sr-only">Details</span></th></tr></thead>
            <tbody>{shownRows.map(({ target, status }) => <tr key={target.target_id} className={`approval-row approval-row--${status.kind}${selected?.target_id === target.target_id ? ' is-selected' : ''}`}>
              <td>{target.score ? <ScoreBadge score={target.score} /> : <span className="approval-score approval-score--none">–</span>}</td>
              <td><div className="approval-pr-title">#{target.number} · {title(target)}</div><div className="approval-pr-meta">{target.owner}/{target.repo}{status.reason && <> · <span className="approval-reason">{status.reason}</span></>}</div></td>
              <td><span className={`approval-status approval-status--${status.kind}`}>{status.label}</span></td>
              <td><button className="approval-link" onClick={event => showEvidence(target, event.currentTarget)}>View evidence</button></td>
            </tr>)}</tbody></table>}
        </div>
        {selected && <aside className="approval-evidence" ref={evidenceRef} tabIndex={-1} aria-label={`Evidence for PR ${selected.number}`} onKeyDown={event => { if (event.key === 'Escape') { event.stopPropagation(); closeEvidence(); } }}>
          <div className="approval-evidence-head"><div><h3>#{selected.number} · {title(selected)}</h3><span className="approval-muted">{selected.owner}/{selected.repo}</span></div><button onClick={closeEvidence} aria-label="Close evidence">Close</button></div>
          {(!selectedPR || selectedPR.hidden || !latestSelected) && <p role="status">This result is no longer in your current dashboard. Historical evidence is unavailable here.</p>}
          {detail.isPending && <p role="status">Loading evidence...</p>}{detail.error && !isCancelledError(detail.error) && <p role="alert">{detail.error.message}</p>}
          {currentDetail && <EvidenceDetail target={currentDetail} status={rowStatus(currentDetail, now)} commit={selectedPR?.commit_sha} actions={<><button disabled={busy || !!active || !capabilities.data?.available} onClick={() => recheck(currentDetail)} title={active ? 'Investigation already running' : undefined}>Recheck</button><a className="approval-button-link" href={`https://github.com/${encodeURIComponent(currentDetail.owner)}/${encodeURIComponent(currentDetail.repo)}/pull/${currentDetail.number}`} target="_blank" rel="noreferrer">Open PR</a></>} />}
        </aside>}
      </div>}
    </section>
  </>;
}

type RowKind = 'candidate' | 'close' | 'blocked' | 'work' | 'other';
const blockerText: Record<string, string> = { ci_failed: 'CI failing', human_changes_requested: 'Changes requested', provider_changes_requested: 'Review tool requested changes', pr_draft: 'Draft', ci_pending: 'CI still running', review_in_progress: 'Review in progress', source_incomplete: 'Evidence incomplete', already_approved: 'Already approved', review_missing: 'No review of the current commit' };
const dispositionText: Record<string, string> = { fixed: 'Fixed', not_applicable: 'Does not apply', still_present: 'Still present', style_only: 'Style only', cannot_tell: 'Unclear', unresolved: 'Unresolved', uncertain: 'Uncertain', non_blocking: 'Non-blocking' };
const staleText: Record<string, string> = { head_changed: 'New commits pushed', observed_ci_change: 'CI changed', blocker_cleared: 'A blocker cleared', observed_review_change: 'Changes requested', observed_pr_change: 'PR changed', closed: 'PR closed', draft: 'Draft' };
const staleBlockers = new Set(['ci_failed', 'ci_pending', 'observed_ci_change', 'human_changes_requested', 'observed_review_change', 'draft', 'pr_draft']);
const impactText = (impact: number) => impact >= 2.5 ? 'Severe' : impact >= 1.5 ? 'Defect' : impact >= 0.5 ? 'Minor' : 'Cosmetic';

function rowStatus(target: ApprovalTarget, now: number): { kind: RowKind; label: string; reason?: string } {
  const score = target.score || target.assessment?.score || undefined;
  const bucket = approvalBucket(target, now);
  const blockers = score?.blockers || [];
  const topConcern = (score?.concerns || []).find(concern => concern.risk >= 0.05);
  const deductions = (score?.deductions || []).map(d => blockerText[d.reason] || label(d.reason));
  const reason = blockers.length ? blockers.map(code => blockerText[code] || label(code)).join(', ') : topConcern ? [...deductions, cleanClaim(topConcern.claim).text].join(' · ') : deductions.length ? deductions.join(', ') : score ? 'No open concerns' : undefined;
  if (['failed', 'timed_out', 'cancelled'].includes(bucket)) return { kind: 'other', label: label(bucket), reason: target.summary };
  if (bucket === 'excluded') return { kind: 'other', label: 'Excluded', reason: (target.reason_codes || []).map(code => blockerText[code] || label(code)).join(', ') };
  if (isApprovalCandidate(target, now)) return { kind: 'candidate', label: 'Candidate', reason };
  if (bucket === 'stale') {
    const codes = target.reason_codes || [];
    const blocking = codes.filter(code => staleBlockers.has(code));
    const why = codes.filter(code => code in blockerText || code in staleText).map(code => blockerText[code] || staleText[code]).join(', ') || reason;
    if (blocking.length) return { kind: 'blocked', label: 'Blocked', reason: why };
    return { kind: 'other', label: codes.includes('review_in_progress') ? 'Review updating' : target.decision === 'candidate' ? 'Needs recheck' : 'Out of date', reason: why };
  }
  if (blockers.length) return { kind: 'blocked', label: 'Blocked', reason };
  if (score && score.value >= score.threshold * 0.6) return { kind: 'close', label: target.score?.deductions?.some(d => d.reason === 'pr_draft') ? 'Draft' : 'Close', reason };
  if (score) return { kind: 'work', label: 'Needs work', reason };
  return { kind: 'other', label: label(bucket), reason: (target.reason_codes || []).map(code => blockerText[code] || label(code)).join(', ') || target.summary };
}

// cleanClaim turns a review finding into display text: the first-pass marker
// becomes a tag, truncation markers become an ellipsis.
function cleanClaim(claim: string): { text: string; firstPass: boolean } {
  let text = claim.trim();
  const firstPass = /^_?\[first-pass finding[^\]]*\]_?/i.test(text);
  text = text.replace(/^_?\[first-pass finding[^\]]*\]_?\s*/i, '').replace(/\s*\[truncated\]\s*$/i, '…').replace(/\s+/g, ' ');
  return { text, firstPass };
}

function InlineCode({ text }: { text: string }) {
  return <>{text.split(/(`[^`]+`)/g).map((part, index) => part.startsWith('`') && part.endsWith('`') && part.length > 2 ? <code key={index}>{part.slice(1, -1)}</code> : <span key={index}>{part}</span>)}</>;
}

function scoreBand(score: ApprovalScore) {
  if (score.candidate) return 'high';
  return score.value >= score.threshold * 0.6 ? 'mid' : 'low';
}

function ScoreBadge({ score, large }: { score: ApprovalScore; large?: boolean }) {
  return <span className={`approval-score approval-score--${scoreBand(score)}${large ? ' approval-score--large' : ''}`} title={`Approval score ${score.value.toFixed(0)} of 100; candidates need ${score.threshold.toFixed(0)}`}>{score.value.toFixed(0)}</span>;
}

function ConcernCard({ concern }: { concern: ApprovalConcernScore }) {
  const claim = cleanClaim(concern.claim);
  return <article className="approval-finding">
    <div className="approval-finding-meta">
      <span className={`approval-pill approval-pill--${concern.severity}`}>{label(concern.severity)}</span>
      <span className="approval-pill">{dispositionText[concern.disposition] || label(concern.disposition)}</span>
      {claim.firstPass && <span className="approval-pill approval-pill--muted">First pass</span>}
      <span className="approval-muted">{Math.round(100 * (1 - concern.p_resolved))}% likely open · {impactText(concern.impact)}</span>
    </div>
    <p className="approval-finding-text"><InlineCode text={claim.text} /></p>
  </article>;
}

function EvidenceDetail({ target, status, commit, actions }: { target: ApprovalTarget; status: ReturnType<typeof rowStatus>; commit?: string; actions: React.ReactNode }) {
  const score = target.assessment?.score;
  const current = (score?.concerns || []).filter(concern => concern.on_head);
  const older = (score?.concerns || []).filter(concern => !concern.on_head);
  const sources = (target.sources || target.assessment?.sources || []).filter(source => source.provider !== 'generic');
  return <div className="approval-detail">
    <div className="approval-detail-score">
      {score ? <ScoreBadge score={score} large /> : null}
      <div className="approval-detail-summary"><strong className={`approval-status approval-status--${status.kind}`}>{status.label}</strong>{score && <span className="approval-muted">Score {score.value.toFixed(0)} of 100 · candidates need {score.threshold.toFixed(0)}</span>}</div>
      <div className="approval-actions">{actions}</div>
    </div>
    {score ? <section className="approval-detail-section"><h4>Why this score</h4><ul className="approval-why">
      {(score.blockers || []).map(code => <li key={code} className="approval-why--blocker">{blockerText[code] || label(code)}: caps the score at 20</li>)}
      {(score.deductions || []).map(d => <li key={d.reason}>{blockerText[d.reason] || label(d.reason)}: −{d.points}</li>)}
      {!score.blockers?.length && !score.deductions?.length && !current.some(c => c.risk >= 0.05) && <li>No blockers and no open findings on the current commit.</li>}
      {current.some(c => c.risk >= 0.05) && <li>{current.filter(c => c.risk >= 0.05).length} open findings on the current commit lower the score.</li>}
    </ul></section> : <p>{target.assessment?.summary || target.summary}</p>}
    {!!current.length && <section className="approval-detail-section"><h4>Findings on the current commit ({current.length})</h4>{current.map(concern => <ConcernCard key={concern.id} concern={concern} />)}</section>}
    {!!older.length && <details className="approval-detail-section"><summary>Findings from earlier commits ({older.length}) · usually already addressed</summary>{older.map(concern => <ConcernCard key={concern.id} concern={concern} />)}</details>}
    {!score && !!target.assessment?.concerns?.length && <section className="approval-detail-section"><h4>Concerns</h4>{target.assessment.concerns.map(concern => <article className="approval-finding" key={concern.id}><div className="approval-finding-meta"><span className="approval-pill">{dispositionText[concern.disposition] || label(concern.disposition)}</span></div><p className="approval-finding-text"><InlineCode text={cleanClaim(concern.claim).text} /></p><p className="approval-muted">{concern.rationale}</p>{(concern.citations || []).map((citation, index) => <Citation key={index} citation={citation} target={target} />)}</article>)}</section>}
    <details className="approval-detail-section"><summary>Review sources and checks</summary>
      <ul className="approval-plain">{sources.map(source => <li key={source.id}><strong>{source.provider}</strong> · {label(source.completion)} · {source.reviewed_sha ? (source.reviewed_sha === commit ? 'current commit' : `commit ${source.reviewed_sha.slice(0, 7)}`) : 'commit unknown'}{source.verified ? '' : ' · unverified'}</li>)}</ul>
      <h4>Checks and limitations</h4>
      <ul className="approval-plain">{(target.snapshot?.checks || []).map((check, index) => <li key={index}>{check.name}: {label(check.state)}</li>)}{(target.assessment?.coverage_gaps || []).map((gap, index) => <li key={`gap-${index}`}>{gap}</li>)}{(target.reason_codes || []).map(reason => <li key={reason}>{blockerText[reason] || label(reason)}</li>)}</ul>
      <p className="approval-muted">Assessed commit <code>{target.revision.slice(0, 12)}</code> · {time(target.assessment?.assessed_at)}{target.assessment?.origin === 'reused' ? ' · reused an identical earlier result' : ''}</p>
    </details>
  </div>;
}
