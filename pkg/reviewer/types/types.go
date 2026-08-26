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
}
