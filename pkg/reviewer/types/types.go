package types

// LineComment defines the structure for a single line-specific review comment.
type LineComment struct {
	FilePath        string           `json:"file_path"`
	LineNumber      int              `json:"line_number"`
	CommentBody     string           `json:"comment_body"`
	Importance      string           `json:"importance,omitempty"`
	FindingContract *FindingContract `json:"finding_contract,omitempty"`
	// Provenance identifies which review pass produced this comment:
	// "agent", "first-pass", "mechanical", "required-check", or "carried".
	// Optional and additive: producers that predate it leave it empty, and
	// consumers fall back to the merge layer's prose provenance markers
	// (payload.DeriveProvenance). Empty means unattributed, which downstream
	// consumers treat as the canonical agent voice.
	Provenance string `json:"provenance,omitempty"`

	// ID is the agent's own short label for a finding ("A-1"); it lets the
	// SUMMARY's priority list and other findings refer to it before the
	// server assigns fingerprints.
	ID string `json:"id,omitempty"`
	// Sources lists the first-pass claim ids ("FP-3") this finding confirms
	// or covers, so those claims are recorded as merged into it.
	Sources []string `json:"sources,omitempty"`
	// Disposition marks an entry that is not a finding but the agent's
	// verdict on one first-pass claim (today only "rejected" with a reason).
	Disposition *Disposition `json:"disposition,omitempty"`
	// Summary is the structured form of the SUMMARY entry; when present the
	// service renders CommentBody from it.
	Summary *SummaryBlock `json:"summary,omitempty"`

	// State is what the review concluded about the finding: "confirmed",
	// "unverified", "rejected" or "merged". Empty means confirmed.
	State string `json:"state,omitempty"`
	// Inactive findings are records the review keeps but does not assert as
	// claims: rejected or unexamined first-pass claims and merged aliases.
	// They are excluded from counts, publication and benchmark scoring.
	Inactive bool `json:"inactive,omitempty"`
	// Assessment is the agent's proposal for a first-pass claim when policy
	// kept the claim active anyway (a disputed finding).
	Assessment *Disposition `json:"assessment,omitempty"`
	// Original preserves a first-pass claim as it was handed to the agent.
	Original *OriginalClaim `json:"original,omitempty"`
	// MergedInto is the id of the finding a merged claim was folded into, and
	// MergeBasis says why: "sources" when the agent listed the claim, or
	// "proximity" when the same-file nearby-line heuristic folded it.
	MergedInto string `json:"merged_into,omitempty"`
	MergeBasis string `json:"merge_basis,omitempty"`
	// Verification is the repo-evidence verifier's decision on this finding;
	// nil when the verifier did not run.
	Verification *Verification `json:"verification,omitempty"`
	// Display is the short title and direct body the inline comment shows in
	// place of the headline and impact; CommentBody, and so the finding's
	// identity, is unchanged. Nil when no rewrite ran.
	Display *Display `json:"display,omitempty"`
}

// Display is a finding rewritten for posting.
type Display struct {
	Title string `json:"title"`
	Body  string `json:"body"`
}

// Verification statuses.
const (
	VerifyKeep        = "keep"
	VerifyRevise      = "revise"
	VerifyHold        = "hold"
	VerifyDrop        = "drop"
	VerifyUnavailable = "unavailable"
	VerifySkipped     = "skipped"
	VerifyFailed      = "failed"
)

// Verification is the verifier's decision on one merged finding. It is kept
// apart from State because any State other than confirmed is unpublishable.
// Applied is false in shadow mode, where publishers must ignore the decision.
// LowValue marks a finding that was LOW before verification and that the
// agent judged not worth posting (true but not actionable); it keeps the
// note off GitHub like a hold, with ValueReason saying why. A MEDIUM judged
// the same way is lowered to LOW instead, with DemotedFrom set. Phase is 2 when the escalation agent made the decision. Proposed is the
// model's decision when policy recorded a different
// Status (an invalid decision, or a hold or drop on a CRITICAL). Cites are
// evidence references: packet item ids (P3) from the one-call check, or
// path:line and base:path:line from the agent; Evidence quotes the cited text so a
// withdrawal can show it without the packet. For a revise, the finding
// already carries the replacement and Prior* hold what it replaced.
type Verification struct {
	Status       string   `json:"status"`
	Phase        int      `json:"phase,omitempty"`
	Proposed     string   `json:"proposed,omitempty"`
	Applied      bool     `json:"applied"`
	Reason       string   `json:"reason,omitempty"`
	Cites        []string `json:"cites,omitempty"`
	Evidence     string   `json:"evidence,omitempty"`
	Model        string   `json:"model,omitempty"`
	PriorComment string   `json:"prior_comment,omitempty"`
	LowValue     bool     `json:"low_value,omitempty"`
	ValueReason  string   `json:"value_reason,omitempty"`
	// Gate is the pre-publish quality gate's grade (GOOD, OK or BAD) on the
	// finding as it would post; GateApplied is false in shadow mode. A BAD
	// grade keeps the finding off GitHub, except that a CRITICAL is posted as
	// a LOW note instead (GateDemoted).
	Gate         string `json:"gate,omitempty"`
	GateCategory string `json:"gate_category,omitempty"`
	GateReason   string `json:"gate_reason,omitempty"`
	GateApplied  bool   `json:"gate_applied,omitempty"`
	GateDemoted  bool   `json:"gate_demoted,omitempty"`
	// DemotedFrom is the severity a finding judged not worth posting had
	// before it was posted as a LOW note instead.
	DemotedFrom   string           `json:"demoted_from,omitempty"`
	PriorContract *FindingContract `json:"prior_contract,omitempty"`
}

// Suppresses reports whether an applied decision keeps the finding from
// being posted as new.
func (v *Verification) Suppresses() bool {
	return v != nil && (v.Applied && (v.Status == VerifyHold || v.Status == VerifyDrop || v.LowValue) || v.GateDrops())
}

// GateDrops reports whether the applied quality gate keeps the finding off GitHub.
func (v *Verification) GateDrops() bool {
	return v != nil && v.GateApplied && v.Gate == "BAD" && !v.GateDemoted
}

// Refutes reports whether an applied decision withdraws the finding.
func (v *Verification) Refutes() bool {
	return v != nil && v.Applied && v.Status == VerifyDrop
}

// Disposition is the agent's verdict on one first-pass claim.
type Disposition struct {
	SourceID string        `json:"source_id"`
	State    string        `json:"state"`
	Reason   string        `json:"reason,omitempty"`
	Evidence []EvidenceRef `json:"evidence,omitempty"`
}

// EvidenceRef points at code the agent read to reach a disposition.
type EvidenceRef struct {
	File string `json:"file"`
	Line int    `json:"line,omitempty"`
}

// OriginalClaim is a first-pass claim as originally stated.
type OriginalClaim struct {
	SourceID   string `json:"source_id"`
	FilePath   string `json:"file_path"`
	LineNumber int    `json:"line_number"`
	Importance string `json:"importance,omitempty"`
	Comment    string `json:"comment"`
}

// SummaryBlock is the structured SUMMARY the agent emits.
type SummaryBlock struct {
	Verdict     string   `json:"verdict"`
	Upshot      string   `json:"upshot,omitempty"`
	PriorityIDs []string `json:"priority_ids,omitempty"`
	Notes       string   `json:"notes,omitempty"`
}
