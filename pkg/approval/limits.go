package approval

import (
	"errors"
	"fmt"
)

// Limit codes name the resource ceiling that stopped an investigation; they
// are stored as reason codes so a limited run says which limit it reached.
const (
	LimitBudget       = "budget_exhausted"
	LimitConversation = "conversation_limit"
	LimitEvidence     = "evidence_limit"
	LimitToolResult   = "tool_result_limit"
)

// Per-target resource ceilings. They guard against runaway investigations,
// not spend; the target's time budget is the binding limit in practice. The
// conversation cap (about four bytes per token) stays inside the per-call
// input allowance, and both sit well inside the model's 1M-token context.
const (
	TargetInputTokens  = 2_000_000
	TargetOutputTokens = 160_000
	CallInputTokens    = 400_000
	// CallOutputTokens bounds one reply, reasoning included, so a runaway
	// reasoning trace cannot consume the target's time budget; a follow-up
	// for a few missing entries gets FollowUpOutputTokens.
	CallOutputTokens     = 32_000
	FollowUpOutputTokens = 6_000
	// DefaultReasoningTokens caps the model's hidden reasoning per call; an
	// effort level alone left it at 5-14k tokens and dominated wall time.
	DefaultReasoningTokens = 6_000
	MaxRounds              = 8
	MaxToolCalls           = 16
	MaxToolBytes           = 1 << 20
	// MaxToolRounds and MaxToolCallsPerRound bound optional reads in one
	// attempt; the evidence, diff and anchor code are already preloaded.
	MaxToolRounds        = 1
	MaxToolCallsPerRound = 8
	// Citation checks read the cited code after the model answers; they get
	// their own allowance so a long investigation cannot starve them.
	MaxCitationReads = 200
	MaxCitationBytes = 8 << 20
	// maxToolResultBytes bounds one tool result returned to the model.
	maxToolResultBytes = 65536

	maxConversationBytes = 1_400_000

	// Preload budgets for the first prompt. Evidence over its budget is a
	// limit; diff and code past theirs are left to the read tools.
	PreloadEvidenceBytes = 600_000
	PreloadDiffBytes     = 200_000
	PreloadCodeBytes     = 240_000
)

type limitError struct {
	code   string
	detail string
}

func (e *limitError) Error() string {
	return fmt.Sprintf("%s: %s: %s", ErrInvestigationLimit, e.code, e.detail)
}

func (e *limitError) Is(target error) bool { return target == ErrInvestigationLimit }

func investigationLimit(code, format string, args ...any) error {
	return &limitError{code: code, detail: fmt.Sprintf(format, args...)}
}

// LimitCode returns the limit that err reports, or LimitBudget for a limit
// error that does not name one (a provider-reported usage limit).
func LimitCode(err error) string {
	var le *limitError
	if errors.As(err, &le) {
		return le.code
	}
	return LimitBudget
}

// LimitDescription names a limit code for people.
func LimitDescription(code string) string {
	switch code {
	case LimitConversation:
		return "conversation size limit"
	case LimitEvidence:
		return "evidence size limit"
	case LimitToolResult:
		return "tool result size limit"
	default:
		return "token and tool budget"
	}
}
