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
	if medium > 0 {
		score--
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

var hunkRangeRe = regexp.MustCompile(`^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@`)

// ParseHunks lists the hunks of a unified-diff patch from their headers: the
// base-side and head-side ranges each change covers, context included. A
// count GitHub omits is one line.
func ParseHunks(patch string) []Hunk {
	var hunks []Hunk
	for _, raw := range strings.Split(patch, "\n") {
		m := hunkRangeRe.FindStringSubmatch(raw)
		if m == nil {
			continue
		}
		h := Hunk{OldLines: 1, NewLines: 1}
		h.OldStart, _ = strconv.Atoi(m[1])
		h.NewStart, _ = strconv.Atoi(m[3])
		if m[2] != "" {
			h.OldLines, _ = strconv.Atoi(m[2])
		}
		if m[4] != "" {
			h.NewLines, _ = strconv.Atoi(m[4])
		}
		hunks = append(hunks, h)
	}
	return hunks
}

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

// ChangedLines returns the RIGHT-side line numbers a patch adds or edits:
// the "+" lines of every hunk, which is where a finding's anchor moves when
// the code under it changes.
func ChangedLines(patch string) map[int]bool {
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
		case strings.HasPrefix(raw, "+"):
			lines[right] = true
			right++
		default:
			right++
		}
	}
	return lines
}
