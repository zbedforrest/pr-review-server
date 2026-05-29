// Package payload defines the JSON shape served by /api/review and persisted
// as a sidecar next to the rendered HTML in GCS. It is the structured form of
// a review — what the HTML report renders, but in a shape an LLM/CLI consumer
// can parse without scraping markup.
//
// The package is intentionally dependency-light (types + pure helpers) so both
// the poller (write side) and the HTTP server (read side) can import it
// without import cycles.
package payload

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"pr-review-server/pkg/reviewer/types"
)

// SourceContextLines is the default number of lines of surrounding source
// included before and after each finding's cited line. Picked to be generous
// enough that the consumer rarely needs a follow-up file read for context,
// while keeping payloads small.
const SourceContextLines = 12

// Payload is the on-the-wire JSON shape returned by GET /api/review/... and
// persisted as the .json sidecar in GCS.
//
// Schema version bumps when we make breaking changes; consumers may read
// SchemaVersion to gate their handling. Current: "1".
type Payload struct {
	SchemaVersion string `json:"schema_version"`

	Owner     string `json:"owner"`
	Repo      string `json:"repo"`
	PRNumber  int    `json:"pr_number"`
	CommitSHA string `json:"commit_sha"`

	Counts   Counts    `json:"counts"`
	Findings []Finding `json:"findings"`
}

// Counts is the per-severity tally. Mirrors the columns on the PR row.
type Counts struct {
	Critical int `json:"critical"`
	Medium   int `json:"medium"`
	Low      int `json:"low"`
}

// Finding is one reviewer comment with enough surrounding context that an
// LLM consumer can act on it without re-fetching the file.
//
// `DiffHunk` is the unified-diff hunk containing the cited line (the hunk
// header plus its ` `/`+`/`-` body). `SourceBefore` / `SourceAfter` are
// SourceContextLines lines of the post-image file around `Line` (1-indexed,
// inclusive of the cited line in SourceBefore). The fields together let a
// consumer reconstruct "what changed at this line + what surrounds it" in
// one trip.
type Finding struct {
	Severity     string   `json:"severity"` // critical | medium | low | unknown
	File         string   `json:"file"`
	Line         int      `json:"line"`
	Comment      string   `json:"comment"`
	DiffHunk     string   `json:"diff_hunk,omitempty"`
	SourceBefore []string `json:"source_before,omitempty"`
	SourceAfter  []string `json:"source_after,omitempty"`
}

// normalizeSeverity lower-cases LineComment importance values into the four
// stable strings consumers should branch on.
func normalizeSeverity(imp string) string {
	switch strings.ToUpper(strings.TrimSpace(imp)) {
	case "CRITICAL":
		return "critical"
	case "MEDIUM":
		return "medium"
	case "LOW":
		return "low"
	default:
		return "unknown"
	}
}

// Build assembles a Payload from the structured outputs of a review run.
//
//   - comments: the structured LineComments the reviewer produced.
//   - diff: the unified diff used for the review (for hunk extraction).
//   - fileContents: post-image file contents keyed by path (for source windows).
//     Pass nil if file contents weren't captured — source windows will be empty.
//
// Findings are sorted critical → medium → low → unknown, then by file/line
// for stable output.
func Build(
	owner, repo string,
	prNumber int,
	commitSHA string,
	comments []types.LineComment,
	diff string,
	fileContents map[string]string,
) Payload {
	findings := make([]Finding, 0, len(comments))
	var counts Counts

	for _, c := range comments {
		sev := normalizeSeverity(c.Importance)
		switch sev {
		case "critical":
			counts.Critical++
		case "medium":
			counts.Medium++
		case "low":
			counts.Low++
		}

		f := Finding{
			Severity: sev,
			File:     c.FilePath,
			Line:     c.LineNumber,
			Comment:  c.CommentBody,
		}
		if diff != "" && c.FilePath != "" && c.LineNumber > 0 {
			f.DiffHunk = HunkForLine(diff, c.FilePath, c.LineNumber)
		}
		if fileContents != nil {
			if src, ok := fileContents[c.FilePath]; ok {
				f.SourceBefore, f.SourceAfter = SourceWindow(src, c.LineNumber, SourceContextLines)
			}
		}
		findings = append(findings, f)
	}

	sort.SliceStable(findings, func(i, j int) bool {
		if findings[i].Severity != findings[j].Severity {
			return severityRank(findings[i].Severity) < severityRank(findings[j].Severity)
		}
		if findings[i].File != findings[j].File {
			return findings[i].File < findings[j].File
		}
		return findings[i].Line < findings[j].Line
	})

	return Payload{
		SchemaVersion: "1",
		Owner:         owner,
		Repo:          repo,
		PRNumber:      prNumber,
		CommitSHA:     commitSHA,
		Counts:        counts,
		Findings:      findings,
	}
}

// CompactMeta carries the request-time envelope fields that live outside the
// persisted Payload (head SHA, staleness, etc.) but appear in the compact
// Markdown header. The HTTP layer fills these from the review API envelope.
type CompactMeta struct {
	HeadSHA           string
	IsStale           bool
	IsInFlight        bool
	GeneratedAt       string // pre-formatted; "" renders as "unknown"
	ReviewURL         string
	FindingsAvailable bool
}

// ToCompactMarkdown renders the review as a compact, self-contained Markdown
// document intended for pasting into a coding agent. Each finding carries its
// severity, location, the reviewer comment, the unified-diff hunk, and the
// surrounding source window — enough to act on without re-fetching files.
//
// This is the single canonical server-side formatter; it mirrors the layout
// the prism-review skill renders client-side so the two can converge.
func (p Payload) ToCompactMarkdown(meta CompactMeta) string {
	var b strings.Builder

	schema := p.SchemaVersion
	if schema == "" {
		schema = "unknown"
	}
	gen := meta.GeneratedAt
	if gen == "" {
		gen = "unknown"
	}

	fmt.Fprintf(&b, "PR: %s/%s#%d\n", p.Owner, p.Repo, p.PRNumber)
	fmt.Fprintf(&b, "Reviewed commit: %s\n", p.CommitSHA)
	fmt.Fprintf(&b, "Current HEAD:    %s\n", meta.HeadSHA)
	fmt.Fprintf(&b, "Stale: %t\n", meta.IsStale)
	fmt.Fprintf(&b, "In flight (regenerating): %t\n", meta.IsInFlight)
	fmt.Fprintf(&b, "Generated at: %s\n", gen)
	fmt.Fprintf(&b, "Counts: critical=%d medium=%d low=%d\n", p.Counts.Critical, p.Counts.Medium, p.Counts.Low)
	fmt.Fprintf(&b, "Review URL: %s\n", meta.ReviewURL)
	fmt.Fprintf(&b, "Schema: %s\n\n", schema)

	if !meta.FindingsAvailable {
		b.WriteString("=== NO STRUCTURED FINDINGS ===\n\n")
		b.WriteString("This review was generated before the JSON sidecar was available, or\n")
		b.WriteString("the sidecar could not be loaded. Open the Review URL above for the HTML\n")
		b.WriteString("report, or regenerate the review.\n")
		return b.String()
	}

	fmt.Fprintf(&b, "=== FINDINGS (%d) ===\n", len(p.Findings))
	for _, f := range p.Findings {
		fmt.Fprintf(&b, "\n--- [%s] %s:%d ---\n\n", strings.ToUpper(f.Severity), f.File, f.Line)
		b.WriteString("COMMENT:\n")
		b.WriteString(strings.TrimRight(f.Comment, "\n"))
		b.WriteString("\n")

		if f.DiffHunk != "" {
			b.WriteString("\nDIFF HUNK:\n```diff\n")
			b.WriteString(strings.TrimRight(f.DiffHunk, "\n"))
			b.WriteString("\n```\n")
		}

		if len(f.SourceBefore) > 0 || len(f.SourceAfter) > 0 {
			b.WriteString("\nSOURCE CONTEXT (cited line is the LAST line before the marker):\n```\n")
			for _, ln := range f.SourceBefore {
				b.WriteString(ln)
				b.WriteString("\n")
			}
			b.WriteString("----- cited line above -----\n")
			for _, ln := range f.SourceAfter {
				b.WriteString(ln)
				b.WriteString("\n")
			}
			b.WriteString("```\n")
		}
	}

	return b.String()
}

func severityRank(sev string) int {
	switch sev {
	case "critical":
		return 0
	case "medium":
		return 1
	case "low":
		return 2
	default:
		return 3
	}
}

// SourceWindow returns up to `context` lines before the cited line (inclusive
// of the cited line itself, as the last element of SourceBefore) and up to
// `context` lines after.
//
// Line is 1-indexed. If `contents` is empty or the line is out of range,
// returns (nil, nil). Trailing newline characters are trimmed per-line so
// the caller doesn't have to.
func SourceWindow(contents string, line, context int) (before, after []string) {
	if contents == "" || line <= 0 || context <= 0 {
		return nil, nil
	}
	// Normalize CRLF → LF so line counting matches what the reviewer saw.
	contents = strings.ReplaceAll(contents, "\r\n", "\n")
	lines := strings.Split(contents, "\n")
	if line > len(lines) {
		return nil, nil
	}
	// Drop a trailing empty element produced by a file ending in \n.
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	if line > len(lines) {
		return nil, nil
	}

	startBefore := line - context
	if startBefore < 1 {
		startBefore = 1
	}
	endAfter := line + context
	if endAfter > len(lines) {
		endAfter = len(lines)
	}

	// SourceBefore: lines [startBefore, line], inclusive of cited line so the
	// consumer always sees the exact text the comment points at even when
	// `context` is 0 (defensive).
	before = append(before, lines[startBefore-1:line]...)
	if line < endAfter {
		after = append(after, lines[line:endAfter]...)
	}
	return before, after
}

// HunkForLine returns the unified-diff hunk in `diff` for `file` that contains
// the given post-image (new-file) line. Returns "" if the diff is malformed,
// the file isn't present, or no hunk covers the line.
//
// The returned string is the raw hunk text: the `@@ ... @@` header followed by
// its ` `/`+`/`-` body lines, joined with "\n". The consumer can render it
// verbatim.
func HunkForLine(diff, file string, newLine int) string {
	if diff == "" || file == "" || newLine <= 0 {
		return ""
	}
	lines := strings.Split(diff, "\n")

	inFile := false
	i := 0
	for i < len(lines) {
		line := lines[i]
		switch {
		case strings.HasPrefix(line, "diff --git "):
			// `diff --git a/path b/path` — match the b/ side (post-image path).
			inFile = diffGitMatchesFile(line, file)
		case strings.HasPrefix(line, "+++ "):
			// Some diffs omit `diff --git`; +++ b/path also identifies the file.
			inFile = plusPlusMatchesFile(line, file)
		case inFile && strings.HasPrefix(line, "@@"):
			newStart, newCount, ok := parseHunkHeader(line)
			if !ok {
				i++
				continue
			}
			// Hunk body extends until the next file header or @@.
			j := i + 1
			for j < len(lines) {
				l := lines[j]
				if strings.HasPrefix(l, "diff --git ") || strings.HasPrefix(l, "@@") || strings.HasPrefix(l, "--- ") || strings.HasPrefix(l, "+++ ") {
					break
				}
				j++
			}
			// newLine falls inside this hunk's post-image range?
			if newLine >= newStart && newLine < newStart+newCount {
				return strings.Join(lines[i:j], "\n")
			}
			i = j
			continue
		}
		i++
	}
	return ""
}

// diffGitMatchesFile reports whether a `diff --git a/X b/Y` line refers to
// the given file path (matching against the b/ side, the post-image path).
func diffGitMatchesFile(header, file string) bool {
	// Format: `diff --git a/<old> b/<new>` — pull off the b/ side.
	idx := strings.Index(header, " b/")
	if idx < 0 {
		return false
	}
	got := header[idx+len(" b/"):]
	// Tolerate trailing tokens (rare but possible in renames-with-flags).
	if sp := strings.IndexByte(got, ' '); sp >= 0 {
		got = got[:sp]
	}
	return got == file
}

// plusPlusMatchesFile reports whether a `+++ b/path` (or `+++ path`) line
// refers to the given file path.
func plusPlusMatchesFile(header, file string) bool {
	rest := strings.TrimPrefix(header, "+++ ")
	rest = strings.TrimPrefix(rest, "b/")
	// Strip a trailing timestamp tab (older diff formats append `\t<ts>`).
	if tab := strings.IndexByte(rest, '\t'); tab >= 0 {
		rest = rest[:tab]
	}
	return rest == file
}

// parseHunkHeader pulls (newStart, newCount) out of `@@ -oldStart,oldCount +newStart,newCount @@ ...`.
// Both counts default to 1 when the `,N` portion is omitted (per unified diff spec).
func parseHunkHeader(line string) (newStart, newCount int, ok bool) {
	// Locate the `+` segment.
	plus := strings.Index(line, "+")
	if plus < 0 {
		return 0, 0, false
	}
	rest := line[plus+1:]
	end := strings.IndexAny(rest, " @")
	if end < 0 {
		return 0, 0, false
	}
	seg := rest[:end]
	// seg is "<start>" or "<start>,<count>"
	startStr, countStr := seg, "1"
	if comma := strings.IndexByte(seg, ','); comma >= 0 {
		startStr = seg[:comma]
		countStr = seg[comma+1:]
	}
	s, err1 := strconv.Atoi(startStr)
	c, err2 := strconv.Atoi(countStr)
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	return s, c, true
}
