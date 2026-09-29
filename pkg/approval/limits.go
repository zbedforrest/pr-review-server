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

// maxConversationBytes caps the serialized conversation sent on each model
// round. At about four bytes per token it stays well inside the 100,000
// input tokens reserved per round, leaving room for the system prompt and
// tool definitions.
const maxConversationBytes = 240000

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
