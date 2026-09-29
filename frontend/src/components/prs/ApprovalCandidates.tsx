import { useEffect, useMemo, useRef, useState } from 'react';
import { createPortal } from 'react-dom';
import { useQuery, useQueryClient, type Query } from '@tanstack/react-query';
import { ApprovalAPIError, approvalRequest, fetchApprovalCapabilities, fetchApprovalScan, fetchApprovalScans, fetchApprovalTargets, targetPath } from '@/api/approval';
import { usePRs } from '@/hooks/usePRs';
import { useCurrentUser } from '@/hooks/useCurrentUser';
import { approvalBucket, approvalKey, approvalScope, isApprovalCandidate, reconcileApprovalTarget, safeEvidenceURL } from '@/utils/approval';
import { subscribeToWebSocketMessages } from '@/utils/websocket';
import type { PR } from '@/types/pr';
import { filterAndSortPRs, type PRFilterCriteria } from '@/utils/sectionFilters';
import type { ApprovalCitation, ApprovalScan, ApprovalTarget } from '@/types/approval';
import '@/styles/components/_approval-candidates.scss';

const activeStates = ['queued', 'running', 'cancelling'];
const terminalStates = ['completed', 'failed', 'timed_out', 'cancelled'];
const label = (value: string) => value.replace(/_/g, ' ');
const scopeDescription = (scope: Record<string, unknown>) => Object.entries(scope).filter(([, value]) => Array.isArray(value) ? value.length > 0 : !!value).map(([key, value]) => `${key}: ${Array.isArray(value) ? value.join(', ') : String(value)}`).join(' · ') || 'All eligible dashboard PRs';
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
  const targets = useQuery({ queryKey: ['approval-targets'], queryFn: fetchApprovalTargets, enabled, refetchInterval: query => query.state.error ? 30000 : scans.data?.scans?.some(scan => activeStates.includes(scan.status)) || Date.now() < validationPollingUntil ? 2000 : 60000 });
  const [selectedScan, setSelectedScan] = useState<string>();
  const [selected, setSelected] = useState<ApprovalTarget>();
  const [open, setOpen] = useState(false);
  const [busy, setBusy] = useState(false);
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
  const scan = useQuery({ queryKey: ['approval-scan', currentScanID], queryFn: () => fetchApprovalScan(currentScanID!), enabled: enabled && !!currentScanID, refetchInterval: query => query.state.error ? 30000 : activeStates.includes(query.state.data?.scan.status || '') ? 2000 : false });
  const detail = useQuery({ queryKey: ['approval-evidence', selected?.target_id], queryFn: () => approvalRequest<ApprovalTarget>(targetPath(selected!)), enabled: enabled && !!selected });
  const scope = useMemo(() => approvalScope(prs, filters, user?.github_username), [prs, filters, user?.github_username]);
  const visiblePRs = useMemo(() => filterAndSortPRs(prs, filters, user?.github_username), [prs, filters, user?.github_username]);
  const visibleKeys = useMemo(() => new Set(visiblePRs.filter(pr => !pr.hidden).map(approvalKey)), [visiblePRs]);
  const prByKey = new Map(prs.map(pr => [approvalKey(pr), pr]));
  const allTargets = (targets.data || []).map(target => { const pr = prByKey.get(approvalKey(target)); return pr ? reconcileApprovalTarget(target, pr) : target; });
  const ordered = new Map(visiblePRs.map((pr, index) => [approvalKey(pr), index]));
  const visible = allTargets.filter(target => visibleKeys.has(approvalKey(target))).sort((a, b) => (ordered.get(approvalKey(a)) || 0) - (ordered.get(approvalKey(b)) || 0));
  const candidates = visible.filter(target => isApprovalCandidate(target, now));
  const others = visible.filter(target => !isApprovalCandidate(target, now));
  const completed = (scan.data?.targets || []).filter(target => terminalStates.includes(target.execution_status)).length;
  const scanCandidates = (scan.data?.targets || []).filter(target => { const pr = prByKey.get(approvalKey(target)); const latest = allTargets.find(item => approvalKey(item) === approvalKey(target)); return pr && latest && isApprovalCandidate(reconcileApprovalTarget(target, pr, latest), now); }).length;
  const overLimit = scope.length > (capabilities.data?.max_targets || 50);
  const selectedPR = selected ? prByKey.get(approvalKey(selected)) : undefined;
  const latestSelected = selected ? allTargets.find(target => approvalKey(target) === approvalKey(selected)) : undefined;
  const currentDetail = detail.data && selectedPR && !selectedPR.hidden && latestSelected
    ? reconcileApprovalTarget(detail.data, selectedPR, latestSelected) : undefined;

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
      client.setQueryData<ApprovalTarget[]>(['approval-targets'], old => old?.map(withdraw));
      client.setQueriesData<ApprovalTarget>({ queryKey: ['approval-evidence'] }, old => old ? withdraw(old) : old);
      client.setQueriesData<{ scan: ApprovalScan; targets: ApprovalTarget[] | null }>({ queryKey: ['approval-scan'] }, old => old ? { ...old, targets: old.targets?.map(withdraw) || null } : old);
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
    } finally { setBusy(false); }
  };
  const launch = () => perform(async () => {
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
  return <>
    {slot && createPortal(<button className="app-header__action-btn" onClick={toggle} aria-expanded={open} aria-controls="approval-candidate-controls">{active ? 'View investigation' : 'Find approval candidates'}</button>, slot)}
    <section id="approval-candidates" ref={sectionRef} className="approval-candidates" aria-label="Approval candidates">
      <h2 className="approval-disclosure-heading" aria-labelledby="approval-disclosure-title"><button type="button" className="approval-disclosure" aria-labelledby="approval-disclosure-title" onClick={toggle} aria-expanded={open} aria-controls="approval-candidate-controls"><span className="approval-disclosure-row"><span id="approval-disclosure-title">Approval candidates <span className="approval-disclosure-count">({candidates.length})</span></span><svg className="approval-disclosure-chevron" aria-hidden="true" viewBox="0 0 20 20" width="20" height="20"><path d="m6 8 4 4 4-4" fill="none" stroke="currentColor" strokeWidth="1.75" strokeLinecap="round" strokeLinejoin="round" /></svg></span>{!open && !allTargets.length && !active && <span className="approval-disclosure-description">{targets.isPending ? 'Loading investigations...' : 'Find PRs where existing review evidence supports a quick human approval decision.'}</span>}</button></h2>
      <div id="approval-candidate-controls" className="approval-launch" hidden={!open}>
        <p>Investigate existing reviews and inspect supporting code for PRs in your current filters.</p>
        <p className="approval-muted">Scope: {filters.repos?.join(', ') || 'All repositories'} · {filters.teams?.join(', ') || 'All teams'} · {filters.states?.join(', ') || 'All states'}{filters.search ? ` · Search: ${filters.search}` : ''}</p>
        {!capabilities.data?.available && <p role="status">{capabilities.data?.unavailable_reason}</p>}
        {overLimit && <p role="status">Narrow your filters to {capabilities.data?.max_targets} PRs or fewer. No PRs will be sampled.</p>}
        {!scope.length && <p>No eligible PRs in these filters. Your own, hidden, draft, closed and already approved current revisions are excluded.</p>}
        <button className="approval-primary" onClick={launch} disabled={busy || !!active || !capabilities.data?.available || overLimit || !scope.length}>Investigate {scope.length} PRs</button>
        {active && <span> Investigation already running.</span>}
      </div>
      {(error || targets.error || scans.error) && <p role="alert">{error || targets.error?.message || scans.error?.message}</p>}
      {scan.data && <div className="approval-progress" role="status" aria-live="polite"><span>Selected {scan.data.scan.kind === 'recheck' ? 'recheck' : 'scan'}: {label(scan.data.scan.status)} · {completed} of {scan.data.scan.total} finished · {scanCandidates} candidates{active ? ' · Investigation still running' : ''}</span>{active && <button disabled={busy || active.cancel_requested} onClick={() => perform(() => approvalRequest(`approval-scans/${active.scan_id}/cancel`, {}))}>{active.cancel_requested ? 'Cancelling remaining' : 'Cancel remaining'}</button>}</div>}
      {scan.data?.scan.scope && <p className="approval-muted">Captured scope: {scopeDescription(scan.data.scan.scope)}</p>}
      {allTargets.length > visible.length && <p className="approval-muted">{allTargets.length - visible.length} results hidden by current filters.</p>}
      {open && !allTargets.length && !active && <p className="approval-muted">{targets.isPending ? 'Loading investigations...' : 'Find PRs where existing review evidence supports a quick human approval decision.'}</p>}
      {!!allTargets.length && !candidates.length && <p>No current approval candidates in this view. Inspect other results for blockers, evidence gaps or expired assessments.</p>}
      <div className={`approval-layout${selected ? ' approval-layout--selected' : ''}`}><div className="approval-list">
        {!!candidates.length && <table><caption className="approval-sr-only">Current approval candidates</caption><thead><tr><th>Pull request</th><th>Why it qualifies</th><th>Evidence</th></tr></thead><tbody>{candidates.map(target => <tr key={target.target_id} className="approval-positive"><td><strong>#{target.number} · {title(target)}</strong><small>{target.owner}/{target.repo}</small></td><td>{target.summary}<div className="approval-chips">{Array.from(new Set((target.sources || []).map(source => source.provider))).map(provider => <span key={provider}>{provider}</span>)}</div><small>Last checked {time(target.validated_at)}</small></td><td><button onClick={event => showEvidence(target, event.currentTarget)}>View evidence</button></td></tr>)}</tbody></table>}
        {!!others.length && <details className="approval-other"><summary>Other results ({others.length})</summary><p className="approval-muted">{Array.from(new Set(others.map(target => approvalBucket(target, now)))).map(bucket => `${label(bucket)}: ${others.filter(target => approvalBucket(target, now) === bucket).length}`).join(' · ')}</p><ul>{others.map(target => <li key={target.target_id}><button onClick={event => showEvidence(target, event.currentTarget)}>#{target.number} · {title(target)}</button><span>{label(approvalBucket(target, now))}</span><small>{(target.reason_codes || []).map(label).join(', ') || target.summary}</small></li>)}</ul></details>}
      </div>
      {selected && <aside className="approval-evidence" ref={evidenceRef} tabIndex={-1} aria-label={`Evidence for PR ${selected.number}`} onKeyDown={event => { if (event.key === 'Escape') { event.stopPropagation(); closeEvidence(); } }}>
        <div className="approval-heading"><h3>#{selected.number} · Review evidence</h3><button onClick={closeEvidence} aria-label="Close evidence">Close</button></div>
        {(!selectedPR || selectedPR.hidden || !latestSelected) && <p role="status">This result is no longer in your current dashboard. Historical evidence is unavailable here.</p>}
        {detail.isPending && <p role="status">Loading evidence...</p>}{detail.error && <p role="alert">{detail.error.message}</p>}
        {currentDetail && <>
          <p><strong>{label(approvalBucket(currentDetail, now))}</strong></p><p>{currentDetail.assessment?.summary || currentDetail.summary}</p>
          <p className="approval-muted">Assessed commit <code>{currentDetail.revision.slice(0, 12)}</code><br />Assessed {time(currentDetail.assessment?.assessed_at)}<br />Last checked {time(currentDetail.validated_at)}</p>
          <div className="approval-actions"><button disabled={busy || !!active || !capabilities.data?.available} onClick={() => recheck(currentDetail)} title={active ? 'Investigation already running' : undefined}>Recheck</button><a href={`https://github.com/${encodeURIComponent(currentDetail.owner)}/${encodeURIComponent(currentDetail.repo)}/pull/${currentDetail.number}`} target="_blank" rel="noreferrer">Open PR</a></div>
          {!!active && <p>Investigation already running. View progress above.</p>}
          <h4>Review sources</h4>{(currentDetail.sources || currentDetail.assessment?.sources || []).map(source => <div className="approval-source" key={source.id}><strong>{source.provider}</strong> · {source.verified ? 'Verified identity' : 'Unverified identity'}<br />{label(source.completion)} · {source.reviewed_sha ? `${source.reviewed_sha === selectedPR?.commit_sha && currentDetail.freshness_state === 'current' ? 'Current commit' : 'Reviewed commit'} ${source.reviewed_sha.slice(0, 12)}` : 'Commit unknown'}<br /><small>{source.file_coverage === 'not_reported' ? 'File coverage not reported' : label(source.file_coverage || 'not_reported')}</small></div>)}
          {!(currentDetail.sources || currentDetail.assessment?.sources || []).some(source => source.provider === 'copilot') && <p className="approval-muted">Copilot: Not observed</p>}
          <h4>Concerns</h4>{(currentDetail.assessment?.concerns || []).map(concern => <article className="approval-concern" key={concern.id}><strong>{concern.claim}</strong><p>Agent assessment: {concern.disposition === 'fixed' ? 'Fix supported by code inspection' : label(concern.disposition)}</p><p>{concern.rationale}</p>{(concern.citations || []).map((citation, index) => <Citation key={index} citation={citation} target={currentDetail} />)}</article>)}
          {!currentDetail.assessment?.concerns?.length && <p>No concern dispositions recorded.</p>}
          <h4>Checks and limitations</h4>{currentDetail.snapshot?.human_changes_requested && <p>Standing human changes requested.</p>}
          <ul>{(currentDetail.snapshot?.checks || []).map((check, index) => <li key={index}>{check.name}: {label(check.state)}</li>)}{(currentDetail.assessment?.coverage_gaps || []).map((gap, index) => <li key={`gap-${index}`}>{gap}</li>)}{(currentDetail.reason_codes || []).map(reason => <li key={reason}>{label(reason)}</li>)}</ul>
          {(currentDetail.assessment?.citations || []).map((citation, index) => <Citation key={index} citation={citation} target={currentDetail} />)}
        </>}
      </aside>}
      </div>
    </section>
  </>;
}
