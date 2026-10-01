import type { ApprovalCapabilities, ApprovalProgress, ApprovalScan, ApprovalTarget } from '@/types/approval';

export class ApprovalAPIError extends Error {
  constructor(message: string, public scanID?: string) { super(message); }
}
export async function approvalRequest<T>(path: string, body?: unknown, key?: string): Promise<T> {
  const response = await fetch(`/api/v1/${path}`, {
    method: body === undefined ? 'GET' : 'POST',
    credentials: 'same-origin',
    headers: body === undefined ? undefined : { 'Content-Type': 'application/json', 'X-PRism-Request': '1', ...(key ? { 'Idempotency-Key': key } : {}) },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  const text = await response.text();
  let data: { error?: { message?: string; scan_id?: string } };
  try {
    data = JSON.parse(text);
  } catch {
    throw new ApprovalAPIError(response.status === 429 ? 'PRism is busy; retrying shortly' : `Investigation request failed (${response.status})`);
  }
  if (!response.ok) throw new ApprovalAPIError(data.error?.message || `Investigation request failed (${response.status})`, data.error?.scan_id);
  return data as T;
}
export const fetchApprovalCapabilities = () => approvalRequest<ApprovalCapabilities>('approval-capabilities');
export async function fetchApprovalTargets(): Promise<ApprovalTarget[]> {
  const result: ApprovalTarget[] = [];
  let cursor = '';
  const seen = new Set<string>();
  do {
    const page = await approvalRequest<{ targets: ApprovalTarget[] | null; next_cursor: string }>(`approval-candidates?limit=100${cursor ? `&cursor=${encodeURIComponent(cursor)}` : ''}`);
    result.push(...(page.targets || []));
    cursor = page.next_cursor;
    if (cursor && seen.has(cursor)) throw new Error('Repeated result page cursor');
    seen.add(cursor);
  } while (cursor);
  return result;
}
export async function fetchApprovalScans(): Promise<{ scans: ApprovalScan[] }> {
  const [recent, full] = await Promise.all([
    approvalRequest<{ scans: ApprovalScan[] | null }>('approval-scans?limit=100'),
    approvalRequest<{ scans: ApprovalScan[] | null }>('approval-scans?kind=full&limit=1'),
  ]);
  const scans = [...(recent.scans || [])];
  for (const scan of full.scans || []) if (!scans.some(item => item.scan_id === scan.scan_id)) scans.push(scan);
  return { scans };
}
export const fetchApprovalScan = (id: string) => approvalRequest<{ scan: ApprovalScan; targets: ApprovalTarget[] | null }>(`approval-scans/${id}?limit=100`);
export const fetchApprovalProgress = (id: string) => approvalRequest<ApprovalProgress>(`approval-scans/${id}/progress`);
export const targetPath = (target: ApprovalTarget) => `approval-scans/${target.scan_id}/targets/${target.target_id}`;
