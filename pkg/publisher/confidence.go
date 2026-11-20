package publisher

import (
	"strings"

	"pr-review-server/pkg/reviewer/payload"
)

// A narrative that requests changes outranks the severity arithmetic, but
// only over findings the PR shows: a verdict resting on findings that were
// all held back has nothing published behind it.
const requestChangesConfidenceCap = 3

// Confidence is the merge-confidence score the summary comment reports:
// MergeConfidence over the Shown findings, capped when the SUMMARY requests
// changes and a Shown medium or critical backs it. Anything else that
// renders the number must call this.
func Confidence(findings []payload.Finding, requiredCheckViolated bool) int {
	critical, medium := 0, 0
	for _, f := range findings {
		if !Shown(f) {
			continue
		}
		switch f.Severity {
		case "critical":
			critical++
		case "medium":
			medium++
		}
	}
	confidence := MergeConfidence(critical, medium, requiredCheckViolated)
	if critical+medium > 0 && confidence > requestChangesConfidenceCap && requestsChanges(findings) {
		confidence = requestChangesConfidenceCap
	}
	return confidence
}

func requestsChanges(findings []payload.Finding) bool {
	for _, f := range findings {
		if f.File == summaryFile {
			body := strings.ToLower(f.Comment)
			return strings.Contains(body, "request changes") || strings.Contains(body, "request-changes")
		}
	}
	return false
}
