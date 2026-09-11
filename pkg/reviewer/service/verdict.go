package service

import (
	"strings"

	"pr-review-server/pkg/reviewer/types"
)

// Review verdict values parsed from the SUMMARY entry. Persisted on the PR
// row by MarkPRCompleted and exposed as review_verdict in the PR JSON; the
// dashboard renders a red "R" badge only for VerdictRequestChanges. The empty
// string means "no parseable verdict" and renders nothing.
const (
	VerdictRequestChanges     = "request_changes"
	VerdictApproveSuggestions = "approve_suggestions"
	VerdictApprove            = "approve"
)

// VerdictFromComments extracts the review's overall verdict from the final
// comment set. The verdict exists only as prose inside the SUMMARY
// pseudo-file entry, so this is a case-insensitive substring match over the
// first SUMMARY body. The match order deliberately mirrors
// benchmark scoring's parse_verdict — most severe phrasing first — so the
// benchmark and the dashboard badge can never disagree about the same body.
//
// Returns "" when there is no SUMMARY entry or its body matches no known
// verdict phrasing (e.g. a parse-fallback prose dump).
func VerdictFromComments(comments []types.LineComment) string {
	var body string
	for _, c := range comments {
		if c.FilePath == "SUMMARY" {
			// A structured summary states its verdict outright; the prose
			// beneath it may mention other verdicts in passing.
			if c.Summary != nil {
				switch strings.ToLower(strings.TrimSpace(c.Summary.Verdict)) {
				case "request_changes":
					return VerdictRequestChanges
				case "approve_suggestions":
					return VerdictApproveSuggestions
				case "approve":
					return VerdictApprove
				}
			}
			body = c.CommentBody
			break
		}
	}
	s := strings.ToLower(body)
	switch {
	case strings.Contains(s, "request changes"), strings.Contains(s, "request-changes"):
		return VerdictRequestChanges
	case strings.Contains(s, "approve with suggestions"), strings.Contains(s, "approve with suggestion"):
		return VerdictApproveSuggestions
	case strings.Contains(s, "approve"):
		return VerdictApprove
	}
	return ""
}
