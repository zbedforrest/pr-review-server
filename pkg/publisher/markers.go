package publisher

import (
	"regexp"
	"strconv"
	"strings"
)

const SummaryMarker = "<!-- prism:summary:v1 -->"

const findingMarkerPrefix = "<!-- prism:finding:"

var findingMarkerRe = regexp.MustCompile(`<!-- prism:finding:([^\s>]+) -->`)

func FindingMarker(id string) string {
	return findingMarkerPrefix + id + " -->"
}

func FindingIDFromBody(body string) (string, bool) {
	m := findingMarkerRe.FindStringSubmatch(body)
	if m == nil {
		return "", false
	}
	return m[1], true
}

func MergeConfidence(critical, medium int, requiredCheckViolated bool) int {
	score := 5
	if critical > 0 {
		score -= 2
	}
	if medium >= 3 {
		score--
	}
	if requiredCheckViolated {
		score--
	}
	if score < 0 {
		return 0
	}
	return score
}

var hunkHeaderRe = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)(?:,\d+)? @@`)

// CommentableLines returns the RIGHT-side line numbers GitHub accepts for a
// review comment on this file: added and context lines inside hunks.
func CommentableLines(patch string) map[int]bool {
	lines := map[int]bool{}
	right := 0
	inHunk := false
	for _, raw := range strings.Split(strings.TrimSuffix(patch, "\n"), "\n") {
		if m := hunkHeaderRe.FindStringSubmatch(raw); m != nil {
			right, _ = strconv.Atoi(m[1])
			inHunk = true
			continue
		}
		if !inHunk || raw == `\ No newline at end of file` {
			continue
		}
		switch {
		case strings.HasPrefix(raw, "-"):
		case strings.HasPrefix(raw, "+"), strings.HasPrefix(raw, " "), raw == "":
			lines[right] = true
			right++
		}
	}
	return lines
}
