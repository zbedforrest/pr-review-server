export interface ApprovalSource {
  id: string; provider: string; login: string; verified: boolean; presence: string;
  completion: string; reviewed_sha: string; revision_relation: string; file_coverage: string;
}
export interface ApprovalCitation {
  evidence_id?: string; revision?: string; path?: string; start_line?: number;
  end_line?: number; excerpt: string; validated: boolean;
}
export interface ApprovalConcern {
  id: string; claim: string; original_severity: string; disposition: string;
  rationale: string; citations: ApprovalCitation[];
}
export interface ApprovalAssessment {
  decision: string; summary: string; reason_codes: string[]; sources: ApprovalSource[];
  concerns: ApprovalConcern[]; coverage_gaps: string[]; citations: ApprovalCitation[]; assessed_at: string;
  origin?: string; reused_from?: string;
}
export interface ApprovalTarget {
  target_id: string; scan_id: string; owner: string; repo: string; number: number;
  sources?: ApprovalSource[];
  revision: string; generation: number; execution_status: string; decision: string;
  freshness_state: string; reason_codes: string[]; summary: string; validated_at: string | null;
  valid_until: string | null; assessment?: ApprovalAssessment | null;
  snapshot?: { evidence: { id: string; url: string; body: string }[]; checks: { name: string; state: string }[]; human_changes_requested: boolean };
}
export interface ApprovalScan { scan_id: string; kind: string; status: string; cancel_requested: boolean; total: number; scope?: Record<string, unknown> }
export interface ApprovalCapabilities { enabled: boolean; available: boolean; unavailable_reason: string; max_targets: number }
export interface ApprovalProgress {
  scan_id: string; status: string; total: number; finished: number;
  running: number; queued: number; summary: string; tool_calls: number;
}
