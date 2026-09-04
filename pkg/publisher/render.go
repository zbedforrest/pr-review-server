package publisher

import (
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"pr-review-server/db"
	"pr-review-server/pkg/reviewer/payload"
)

// SummaryMaxChars stays under GitHub's 65536-char comment body limit.
const SummaryMaxChars = 60000

const (
	SourceTagPrismOnly    = "prism-only"
	SourceTagBoth         = "both"
	SourceTagGreptileOnly = "greptile-only"
)

type GreptileOnlyRef struct {
	Title     string
	File      string
	Line      int
	Severity  string
	CommentID int64
}

type Round struct {
	Owner       string
	Repo        string
	Number      int
	HeadSHA     string
	RoundNumber int

	Findings     []payload.Finding
	SourceTags   map[string]string
	GreptileOnly []GreptileOnlyRef
	// Previous nil means Publish loads the ledger itself.
	Previous []db.PublishedFinding
	// Commentable is file -> RIGHT-side lines a review comment may target,
	// typically built with CommentableLines from the PR file patches.
	Commentable map[string]map[int]bool

	RequiredCheckViolated bool
	DashboardURL          string
	AgentLinkBase         string
}

func (r Round) sourceTag(id string) string {
	if tag, ok := r.SourceTags[id]; ok && tag != "" {
		return tag
	}
	return SourceTagPrismOnly
}

func (r Round) currentFindings() []payload.Finding {
	var out []payload.Finding
	for _, f := range r.Findings {
		if Publishable(f) {
			out = append(out, f)
		}
	}
	return out
}

type roundDiff struct {
	New       int
	StillOpen int
	Fixed     int
}

func (r Round) diff() roundDiff {
	present := map[string]bool{}
	for _, f := range r.currentFindings() {
		present[f.ID] = true
	}
	published := map[string]bool{}
	var d roundDiff
	for _, p := range r.Previous {
		if p.Kind != db.PublishedKindFinding && p.Kind != db.PublishedKindAnnotation {
			continue
		}
		published[p.Fingerprint] = true
		switch {
		case present[p.Fingerprint]:
			d.StillOpen++
		case p.State == db.PublishedStateOpen:
			d.Fixed++
		}
	}
	for id := range present {
		if !published[id] {
			d.New++
		}
	}
	return d
}

func recommendation(confidence int) string {
	switch {
	case confidence >= 5:
		return "No blocking findings."
	case confidence == 4:
		return "Minor findings worth a look before merge."
	case confidence == 3:
		return "Findings that should be addressed before merge."
	default:
		return "Significant findings; please address before merge."
	}
}

func sourceLabel(tag string) string {
	switch tag {
	case SourceTagBoth:
		return "Both"
	case SourceTagGreptileOnly:
		return "Greptile"
	default:
		return "PRism"
	}
}

func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

func truncate(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	if max <= 3 {
		return string(r[:max])
	}
	return string(r[:max-3]) + "..."
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

func tableCell(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "|", "\\|"), "\n", " ")
}

type summaryRow struct {
	severity string
	file     string
	line     int
	text     string
	source   string
}

func (row summaryRow) render() string {
	return fmt.Sprintf("| %s | `%s:%d` | %s | %s |", row.severity, row.file, row.line, row.text, row.source)
}

func (r Round) summaryRows() []summaryRow {
	var rows []summaryRow
	for _, f := range r.currentFindings() {
		rows = append(rows, summaryRow{
			severity: f.Severity,
			file:     f.File,
			line:     f.Line,
			text:     tableCell(truncate(firstLine(commentText(f)), 120)),
			source:   sourceLabel(r.sourceTag(f.ID)),
		})
	}
	for _, g := range r.GreptileOnly {
		text := tableCell(truncate(firstLine(g.Title), 120))
		if g.CommentID != 0 {
			text = fmt.Sprintf("[%s](https://github.com/%s/%s/pull/%d#discussion_r%d)", text, r.Owner, r.Repo, r.Number, g.CommentID)
		}
		rows = append(rows, summaryRow{severity: g.Severity, file: g.File, line: g.Line, text: text, source: "Greptile"})
	}
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if ra, rb := severityRank(a.severity), severityRank(b.severity); ra != rb {
			return ra > rb
		}
		if a.file != b.file {
			return a.file < b.file
		}
		return a.line < b.line
	})
	return rows
}

func RenderSummary(r Round, sel Selection) string {
	critical, medium := 0, 0
	for _, f := range r.currentFindings() {
		switch f.Severity {
		case "critical":
			critical++
		case "medium":
			medium++
		}
	}
	confidence := MergeConfidence(critical, medium, r.RequiredCheckViolated)
	if r.requestsChanges() && confidence > requestChangesConfidenceCap {
		confidence = requestChangesConfidenceCap
	}

	var head strings.Builder
	head.WriteString(SummaryMarker + "\n")
	fmt.Fprintf(&head, "### PRism review: merge confidence %d/5\n", confidence)
	head.WriteString(recommendation(confidence) + "\n\n")
	if r.RoundNumber > 1 {
		d := r.diff()
		fmt.Fprintf(&head, "**Since last review:** %d new · %d still open · %d fixed\n\n", d.New, d.StillOpen, d.Fixed)
	}

	rows := r.summaryRows()
	fmt.Fprintf(&head, "<details><summary>Findings (%d)</summary>\n\n", len(rows))
	head.WriteString("| Sev | Where | Finding | Source |\n|---|---|---|---|\n")

	var foot strings.Builder
	foot.WriteString("</details>\n\n<sub>")
	fmt.Fprintf(&foot, "Reviews (%d) · reviewed %s", r.RoundNumber, shortSHA(r.HeadSHA))
	if r.DashboardURL != "" {
		fmt.Fprintf(&foot, ` · <a href="%s">dashboard</a>`, r.DashboardURL)
	}
	foot.WriteString("</sub>\n")

	truncRow := "| ... | | %d more, see dashboard | |\n"
	if r.DashboardURL != "" {
		truncRow = fmt.Sprintf("| ... | | [%%d more, see dashboard](%s) | |\n", r.DashboardURL)
	}

	budget := SummaryMaxChars - head.Len() - foot.Len()
	var body strings.Builder
	for i, row := range rows {
		line := row.render() + "\n"
		remaining := len(rows) - i
		reserve := 0
		if remaining > 1 {
			reserve = len(truncRow) + 10
		}
		if body.Len()+len(line)+reserve > budget {
			fmt.Fprintf(&body, truncRow, remaining)
			break
		}
		body.WriteString(line)
	}
	return head.String() + body.String() + foot.String()
}

// firstSentence returns the leading sentence of the comment's first line, or
// the whole first line when it has no sentence boundary.
func firstSentence(comment string) string {
	line := firstLine(comment)
	for i := 0; i < len(line); i++ {
		switch line[i] {
		case '.', '!', '?':
			if i+1 == len(line) || line[i+1] == ' ' {
				return line[:i+1]
			}
		}
	}
	return line
}

func RenderInline(f payload.Finding, sourceTag string, agentLinkBase string) string {
	comment := commentText(f)
	sentence := firstSentence(comment)
	title := truncate(sentence, 100)

	body := comment
	if title == sentence {
		if rest := strings.TrimSpace(strings.TrimPrefix(comment, sentence)); rest != "" {
			body = rest
		}
	}

	var b strings.Builder
	b.WriteString(FindingMarker(f.ID) + "\n")
	fmt.Fprintf(&b, "**[%s] %s**\n\n", strings.ToUpper(f.Severity), title)
	b.WriteString(body + "\n")

	if c := f.FindingContract; c != nil && c.Falsifiability == "falsifiable" && c.FalsifiableCondition != nil && c.ExpectedObservable != nil {
		fmt.Fprintf(&b, "\n**How to verify:** %s; expect %s.\n", strings.TrimSuffix(strings.TrimSpace(*c.FalsifiableCondition), "."), strings.TrimSuffix(strings.TrimSpace(*c.ExpectedObservable), "."))
	}

	var subs []string
	switch sourceTag {
	case SourceTagBoth:
		subs = append(subs, "<sub>Source: PRism · Both</sub>")
	case SourceTagGreptileOnly:
		subs = append(subs, "<sub>Source: Greptile</sub>")
	}
	if agentLinkBase != "" {
		subs = append(subs, fmt.Sprintf(`<sub><a href="%s">Fix with agent</a></sub>`, agentLink(agentLinkBase, f)))
	}
	if len(subs) > 0 {
		b.WriteString("\n" + strings.Join(subs, "\n") + "\n")
	}
	return b.String()
}

// agentLink appends the finding coordinates to a base that already carries
// the PR coordinates (".../go/agent?o=...&r=...&n=..."), so the redirect can
// name the exact comment without a lookup.
func agentLink(base string, f payload.Finding) string {
	sep := "&"
	if !strings.Contains(base, "?") {
		sep = "?"
	}
	return base + sep + "f=" + url.QueryEscape(f.ID) + "&p=" + url.QueryEscape(f.File) + "&l=" + strconv.Itoa(f.Line)
}

// The merge layer prefixes re-admitted findings with an italic provenance
// note meant for the HTML report; on GitHub the source tag carries that
// information, so the note is dropped before rendering.
var provenanceNoteRe = regexp.MustCompile(`^_\[[^\]]*\]_\s*`)

func commentText(f payload.Finding) string {
	return strings.TrimSpace(provenanceNoteRe.ReplaceAllString(strings.TrimSpace(f.Comment), ""))
}

// A narrative that requests changes outranks the severity arithmetic: the
// score can never read as "no blocking findings" while the verdict blocks.
const requestChangesConfidenceCap = 3

func (r Round) requestsChanges() bool {
	for _, f := range r.Findings {
		if f.File == "SUMMARY" {
			body := strings.ToLower(f.Comment)
			return strings.Contains(body, "request changes") || strings.Contains(body, "request-changes")
		}
	}
	return false
}
