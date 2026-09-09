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
