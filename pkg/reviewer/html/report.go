package html

import (
	"bytes"
	"embed"
	"fmt"
	"html"
	"html/template"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/gomarkdown/markdown"
	"github.com/microcosm-cc/bluemonday"
	"pr-review-server/pkg/reviewer/payload"
	"pr-review-server/pkg/reviewer/types"
)

//go:embed templates
var templateFS embed.FS

// mdSanitizer strips dangerous HTML (script tags, event handlers, etc.) from
// rendered Markdown so that untrusted input (PR descriptions, AI review text)
// cannot inject script into the review page.
var mdSanitizer = bluemonday.UGCPolicy()

// renderMarkdown converts Markdown to HTML and sanitizes the result. Use this
// for genuine Markdown fields. Do NOT use it for code or diff text — that is
// not Markdown and must be passed through html.EscapeString instead.
func renderMarkdown(s string) template.HTML {
	unsafe := markdown.ToHTML([]byte(s), nil, nil)
	return template.HTML(mdSanitizer.SanitizeBytes(unsafe))
}

// Line represents a single line in a diff.
type Line struct {
	Type          string
	Content       template.HTML
	OldLineNumber int
	NewLineNumber int
}

// DiffFile represents the diff for a single file.
type DiffFile struct {
	Path  string
	Lines []Line
}

// CommentView is a wrapper for a comment with additional view-related data.
// CommentBody has the legacy provenance preface stripped.
type CommentView struct {
	types.LineComment
	Counter      int
	Total        int
	ContextLines []ContextLine // For adjacent comments, lines around the commented line
	AnchorID     string        // "finding-<counter>", the target of the findings-index link
	StatusPill   string        // "" for confirmed findings
	StatusClass  string
}

// ShowsSeverity is false for the summary and check entries, whose headers
// carry no severity pill.
func (v CommentView) ShowsSeverity() bool {
	return v.FilePath != "SUMMARY" && v.FilePath != "CHECK"
}

// SeverityLabel is the severity shown in a finding's comment header.
func (v CommentView) SeverityLabel() string {
	if !v.ShowsSeverity() {
		return ""
	}
	return severityLabel(v.Importance)
}

// severityLabel normalizes a stored importance for display so the header,
// index, next actions and records all read the same.
func severityLabel(importance string) string {
	return strings.ToUpper(strings.TrimSpace(importance))
}

// SeverityClass returns the CSS-class suffix for the header's severity pill.
func (v CommentView) SeverityClass() string {
	return severityClass(v.SeverityLabel())
}

// severityClass maps a severity to one of the fixed sev-* suffixes styled in
// layout.tmpl. Severities come from provider JSON, so anything else (including
// a value with a space, which html/template would pass into the attribute)
// falls back to the neutral pill.
func severityClass(severity string) string {
	switch strings.ToLower(strings.TrimSpace(severity)) {
	case "critical", "high":
		return "critical"
	case "medium":
		return "medium"
	case "low":
		return "low"
	}
	return "note"
}

// Findings-index groups, in reading order.
const (
	groupConfirmed  = "Confirmed"
	groupNeedsCheck = "Needs verification"
	groupMechanical = "Mechanical signals"
)

// FindingRow is one line of the findings index.
type FindingRow struct {
	AnchorID    string
	Severity    string
	StatusPill  string
	StatusClass string
	FilePath    string
	LineNumber  int
	Title       string
}

// SeverityClass returns the CSS-class suffix for the severity pill.
func (r FindingRow) SeverityClass() string {
	return severityClass(r.Severity)
}

// FindingGroup is one state bucket of the findings index.
type FindingGroup struct {
	Name string
	Rows []FindingRow
}

// RecordView is one inactive record in review details: a first-pass claim
// the agent rejected, never examined, or folded into one of its findings.
type RecordView struct {
	Severity   string
	FilePath   string
	LineNumber int
	Claim      string
	Reason     string
	Evidence   []string
	MergedInto string
	MergeBasis string
	// MergedAnchor links to the absorbing finding when it is on the page.
	MergedAnchor string
}

// SeverityClass returns the CSS-class suffix for the severity pill.
func (r RecordView) SeverityClass() string {
	return severityClass(r.Severity)
}

// Location is "file:line", or the file alone for whole-file records.
func (r RecordView) Location() string {
	return location(r.FilePath, r.LineNumber)
}

func location(file string, line int) string {
	if line > 0 {
		return fmt.Sprintf("%s:%d", file, line)
	}
	return file
}

// FixFirstItem is one entry of the structured SUMMARY's priority list,
// resolved to the finding it names.
type FixFirstItem struct {
	AnchorID   string
	Severity   string
	FilePath   string
	LineNumber int
	Title      string
}

// SeverityClass returns the CSS-class suffix for the severity pill.
func (a FixFirstItem) SeverityClass() string {
	return severityClass(a.Severity)
}

// Location is "file:line", or the file alone for whole-file findings.
func (a FixFirstItem) Location() string {
	return location(a.FilePath, a.LineNumber)
}

const (
	stateConfirmed  = "confirmed"
	stateUnverified = "unverified"
	stateRejected   = "rejected"
	stateMerged     = "merged"
)

var summaryVerdictText = map[string]string{
	"approve":             "Verdict: approve.",
	"approve_suggestions": "Verdict: approve with suggestions.",
	"request_changes":     "Verdict: request changes.",
}

// EvidenceLabel is the human wording of the evidence column.
func (c CheckRecord) EvidenceLabel() string {
	if c.EvidenceResolved {
		return evidenceResolvedLabel
	}
	return "no reference"
}

// VerdictClass returns the CSS-class suffix for the verdict badge.
func (c CheckRecord) VerdictClass() string {
	return strings.ToLower(c.Verdict)
}

// findingGroupFor buckets an active comment for the findings index by the
// state the review recorded. Mechanical signals and unanswered memory checks
// keep their own routing: a required-check VIOLATED synthesis is confirmed,
// while a memory alert re-admitted because its check went unanswered was
// confirmed by nobody. A comment with no recorded state (carried or legacy
// input) falls back to its provenance.
func findingGroupFor(c types.LineComment) (group, pill, class string) {
	prov := payload.DeriveProvenance(c)
	if prov == payload.ProvenanceRequiredCheck && alertUnansweredRe.MatchString(c.CommentBody) {
		return groupNeedsCheck, "UNANSWERED CHECK", "unverified"
	}
	if prov == payload.ProvenanceMechanical {
		return groupMechanical, "MECHANICAL", "mechanical"
	}
	switch c.State {
	case stateConfirmed:
		return groupConfirmed, "", ""
	case stateUnverified:
		return groupNeedsCheck, unverifiedPill(prov, c.Assessment != nil), "unverified"
	}
	return findingGroup(prov)
}

func unverifiedPill(provenance string, disputed bool) string {
	switch {
	case disputed:
		return "FIRST PASS · DISPUTED"
	case provenance == payload.ProvenanceCarried:
		return "CARRIED · UNVERIFIED"
	}
	return "FIRST PASS · UNVERIFIED"
}

// findingGroup buckets a provenance into an index group and its status pill.
// Unknown labels are treated as unverified: truthful attribution beats
// promoting them to confirmed.
func findingGroup(provenance string) (group, pill, class string) {
	switch provenance {
	case payload.ProvenanceAgent, payload.ProvenanceRequiredCheck:
		return groupConfirmed, "", ""
	case payload.ProvenanceMechanical:
		return groupMechanical, "MECHANICAL", "mechanical"
	}
	label := strings.ToUpper(strings.ReplaceAll(provenance, "-", " "))
	return groupNeedsCheck, label + " · UNVERIFIED", "unverified"
}

var (
	verdictLineRe    = regexp.MustCompile(`(?i)^[\s*_#>-]*verdict:`)
	suggestionsParRe = regexp.MustCompile(`(?i)^[\s*_#>-]*suggestions?:`)
	// The label with its leading list/emphasis marks and any emphasis that
	// closed right after it ("**Suggestions:** rest").
	suggestionsLabelRe = regexp.MustCompile(`(?i)^[\s#>-]*([*_]*)suggestions?:\s*([*_]*)\s*`)
	legacyLedgerRe     = regexp.MustCompile(`(?i)^[\s*_]*required checks \(id`)
	legacyReconRe      = regexp.MustCompile(`(?i)^[\s*_-]*reconciliation:\s*\d+ earlier-pass finding`)
	markdownMarksRe    = regexp.MustCompile("\\*\\*|__|\\*|`|^#+\\s*|^[-*]\\s+|\\b_|_\\b")
	sentenceEndRe      = regexp.MustCompile(`^(.*?[.!?])(\s|$)`)
	paragraphBreakRe   = regexp.MustCompile(`\n[ \t]*\n`)
)

const (
	descriptionPreviewRunes = 400
	findingTitleRunes       = 120
)

// summaryParts is a SUMMARY comment split into the pieces the layout renders
// separately.
type summaryParts struct {
	Verdict     string
	Suggestions string
	Prose       string
}

func splitParagraphs(s string) []string {
	var out []string
	for _, p := range paragraphBreakRe.Split(strings.ReplaceAll(s, "\r\n", "\n"), -1) {
		if strings.TrimSpace(p) != "" {
			out = append(out, strings.TrimRight(p, " \t\n"))
		}
	}
	return out
}

func stripMarkdownMarks(s string) string {
	return strings.TrimSpace(markdownMarksRe.ReplaceAllString(strings.TrimSpace(s), ""))
}

// isLegacyInternal matches the check ledger and reconciliation footer older
// summaries carried, with or without the "---" rule that introduced them.
// stripSuggestionsLabel drops the "Suggestions:" prefix the heading already
// states. Emphasis opened before the label is kept only when it did not close
// right after it, so "**Suggestions: text**" stays balanced as "**text**".
func stripSuggestionsLabel(paragraph string) string {
	m := suggestionsLabelRe.FindStringSubmatch(paragraph)
	if m == nil {
		return paragraph
	}
	rest := paragraph[len(m[0]):]
	if m[1] != "" && m[2] == "" {
		return m[1] + rest
	}
	return rest
}

func isLegacyInternal(paragraph string) bool {
	probe := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(paragraph), "---"))
	return legacyLedgerRe.MatchString(probe) || legacyReconRe.MatchString(probe)
}

// decomposeSummary lifts the verdict line and the suggestions paragraph out
// of the SUMMARY prose and drops the legacy check ledger and reconciliation
// footer (with the "---" rules that introduced them).
func decomposeSummary(body string) summaryParts {
	var parts summaryParts
	var kept []string
	paragraphs := splitParagraphs(body)
	for i, p := range paragraphs {
		trimmed := strings.TrimSpace(p)
		if isLegacyInternal(trimmed) {
			continue
		}
		if trimmed == "---" && i+1 < len(paragraphs) && isLegacyInternal(paragraphs[i+1]) {
			continue
		}
		if parts.Suggestions == "" && suggestionsParRe.MatchString(trimmed) {
			parts.Suggestions = stripSuggestionsLabel(trimmed)
			continue
		}
		if parts.Verdict == "" {
			lines := strings.Split(p, "\n")
			for j, line := range lines {
				if verdictLineRe.MatchString(line) {
					parts.Verdict = stripMarkdownMarks(line)
					p = strings.Join(append(lines[:j:j], lines[j+1:]...), "\n")
					break
				}
			}
			if strings.TrimSpace(p) == "" {
				continue
			}
		}
		kept = append(kept, p)
	}
	parts.Prose = strings.Join(kept, "\n\n")
	return parts
}

// truncateWords cuts s to at most max runes on a word boundary, appending
// "..." when anything was dropped.
func truncateWords(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	runes := []rune(s)
	cut := max
	for cut > 0 && !unicode.IsSpace(runes[cut]) {
		cut--
	}
	if cut == 0 {
		cut = max
	}
	return strings.TrimRight(string(runes[:cut]), " \t\n.,;:") + "..."
}

// findingTitle is the one-line title for the findings index: the contract
// headline when set, else the first sentence of the body.
func findingTitle(c types.LineComment) string {
	if c.FindingContract != nil && strings.TrimSpace(c.FindingContract.Headline) != "" {
		return truncateWords(strings.TrimSpace(c.FindingContract.Headline), findingTitleRunes)
	}
	body := payload.StripProvenanceNote(c.CommentBody)
	first := ""
	for _, line := range strings.Split(body, "\n") {
		if s := stripMarkdownMarks(line); s != "" {
			first = s
			break
		}
	}
	if m := sentenceEndRe.FindStringSubmatch(first); m != nil {
		first = m[1]
	}
	return truncateWords(first, findingTitleRunes)
}

// descriptionView decides whether the PR body renders plainly or folded
// behind a preview.
type descriptionView struct {
	Folded  bool
	Preview template.HTML
	Full    template.HTML
}

func buildDescription(body string) descriptionView {
	paragraphs := splitParagraphs(body)
	full := renderMarkdown(body)
	if len(paragraphs) <= 1 && utf8.RuneCountInString(strings.TrimSpace(body)) <= descriptionPreviewRunes {
		return descriptionView{Full: full}
	}
	preview := ""
	if len(paragraphs) > 0 {
		preview = truncateWords(strings.TrimSpace(paragraphs[0]), descriptionPreviewRunes)
	}
	return descriptionView{Folded: true, Preview: renderMarkdown(preview), Full: full}
}

// ContextLine represents a line of code context for adjacent comments
type ContextLine struct {
	LineNumber int
	Content    template.HTML
	IsTarget   bool // True if this is the line being commented on
}

// ParseDiff parses a raw diff string into a structured format.
func ParseDiff(diff string) []*DiffFile {
	var files []*DiffFile
	lines := strings.Split(diff, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	var currentFile *DiffFile
	var oldLine, newLine int

	for _, line := range lines {
		if strings.HasPrefix(line, "diff --git") {
			if currentFile != nil {
				files = append(files, currentFile)
			}
			parts := strings.Split(line, " ")
			path := strings.TrimPrefix(parts[2], "a/")
			currentFile = &DiffFile{Path: path}
			continue
		}
		if strings.HasPrefix(line, "---") || strings.HasPrefix(line, "+++") || strings.HasPrefix(line, "index") {
			continue
		}
		if currentFile == nil {
			continue
		}

		if strings.HasPrefix(line, "@@") {
			// Parse hunk header like "@@ -1,5 +1,6 @@"
			parts := strings.Split(line, " ")
			oldRange := strings.Split(strings.TrimPrefix(parts[1], "-"), ",")
			newRange := strings.Split(strings.TrimPrefix(parts[2], "+"), ",")
			_, _ = fmt.Sscanf(oldRange[0], "%d", &oldLine)
			_, _ = fmt.Sscanf(newRange[0], "%d", &newLine)
			continue
		}

		switch {
		case strings.HasPrefix(line, "+"):
			currentFile.Lines = append(currentFile.Lines, Line{
				Type:          "added",
				Content:       template.HTML(html.EscapeString(strings.TrimPrefix(line, "+"))),
				NewLineNumber: newLine,
			})
			newLine++
		case strings.HasPrefix(line, "-"):
			currentFile.Lines = append(currentFile.Lines, Line{
				Type:          "deleted",
				Content:       template.HTML(html.EscapeString(strings.TrimPrefix(line, "-"))),
				OldLineNumber: oldLine,
			})
			oldLine++
		default:
			currentFile.Lines = append(currentFile.Lines, Line{
				Type:          "context",
				Content:       template.HTML(html.EscapeString(strings.TrimPrefix(line, " "))),
				OldLineNumber: oldLine,
				NewLineNumber: newLine,
			})
			oldLine++
			newLine++
		}
	}
	if currentFile != nil && len(currentFile.Lines) > 0 {
		files = append(files, currentFile)
	}

	return files
}

// GenerateReport creates an HTML report from a slice of line comments and a diff string.
func GenerateReport(comments []types.LineComment, diff string, prNumber int, prURL string, prBody string, prompt string, commitSHA string, modelName string, promptTokenCount int32, candidatesTokenCount int32, totalTokenCount int32, generatedAt time.Time) (string, error) {
	return GenerateReportWithContext(comments, diff, prNumber, prURL, prBody, prompt, commitSHA, modelName, promptTokenCount, candidatesTokenCount, totalTokenCount, generatedAt, nil)
}

// CheckRecord is one required check as the report shows it in review
// details (mirrors the service layer's record without importing it).
type CheckRecord struct {
	ID               string
	Source           string
	Question         string
	TargetFile       string
	Verdict          string
	Answer           string
	EvidencePath     string
	EvidenceResolved bool
	Unresolved       bool
}

// ReportInput is everything the report renders. Structured fields (title,
// check records, per-finding provenance) replace what used to be scraped out
// of the SUMMARY prose.
type ReportInput struct {
	Comments        []types.LineComment
	Diff            string
	PRNumber        int
	PRURL           string
	PRTitle         string
	PRBody          string
	Prompt          string
	CommitSHA       string
	ModelName       string
	PromptTokens    int32
	CandidateTokens int32
	TotalTokens     int32
	GeneratedAt     time.Time
	FileContents    map[string]string
	Checks          []CheckRecord
}

// GenerateReportFrom renders the review report from a ReportInput.
func GenerateReportFrom(in ReportInput) (string, error) {
	return generateReport(in)
}

// GenerateReportWithContext creates an HTML report with additional file context for adjacent comments and token counting.
func GenerateReportWithContext(comments []types.LineComment, diff string, prNumber int, prURL string, prBody string, prompt string, commitSHA string, modelName string, promptTokenCount int32, candidatesTokenCount int32, totalTokenCount int32, generatedAt time.Time, fileContents map[string]string) (string, error) {
	return generateReport(ReportInput{
		Comments: comments, Diff: diff, PRNumber: prNumber, PRURL: prURL, PRBody: prBody, Prompt: prompt,
		CommitSHA: commitSHA, ModelName: modelName, PromptTokens: promptTokenCount, CandidateTokens: candidatesTokenCount,
		TotalTokens: totalTokenCount, GeneratedAt: generatedAt, FileContents: fileContents,
	})
}

// SummaryView is one SUMMARY comment with the verdict and suggestions lifted
// out of its prose.
type SummaryView struct {
	CommentView
	Prose string
}

// verdictClass colors the verdict block by decision.
func verdictClass(verdict string) string {
	decision := strings.ToLower(strings.TrimSpace(verdict))
	if i := strings.Index(decision, "verdict:"); i >= 0 {
		decision = strings.TrimSpace(decision[i+len("verdict:"):])
	}
	switch {
	case strings.HasPrefix(decision, "request changes"):
		return "changes"
	case strings.HasPrefix(decision, "approve"):
		return "approve"
	}
	return "neutral"
}

// splitRecords separates the comments the review asserts from the inactive
// records it only preserves.
func splitRecords(comments []types.LineComment) (active, records []types.LineComment) {
	for _, c := range comments {
		if c.Inactive {
			records = append(records, c)
		} else {
			active = append(active, c)
		}
	}
	return active, records
}

// recordViews turns the inactive records of one state into review-details rows.
func recordViews(records []types.LineComment, state string, anchors map[string]string) []RecordView {
	var out []RecordView
	for _, c := range records {
		if c.State != state {
			continue
		}
		v := RecordView{Severity: severityLabel(c.Importance), FilePath: c.FilePath, LineNumber: c.LineNumber,
			Claim: payload.StripProvenanceNote(c.CommentBody), MergedInto: c.MergedInto, MergeBasis: c.MergeBasis, MergedAnchor: anchors[c.MergedInto]}
		if c.Original != nil && strings.TrimSpace(c.Original.Comment) != "" {
			v.Claim = c.Original.Comment
		}
		if c.Assessment != nil {
			v.Reason = strings.TrimSpace(c.Assessment.Reason)
			for _, e := range c.Assessment.Evidence {
				v.Evidence = append(v.Evidence, location(e.File, e.Line))
			}
		}
		out = append(out, v)
	}
	return out
}

// fixFirst resolves the structured SUMMARY's priority ids against the
// findings on the page; ids that name nothing rendered are skipped. The list
// is the reviewer's short list, so it is dropped when it would repeat the full
// findings index rather than shorten it.
func fixFirst(ids []string, byID map[string]CommentView, totalFindings int) []FixFirstItem {
	actions := fixFirstItems(ids, byID)
	if len(actions) >= totalFindings {
		return nil
	}
	return actions
}

func fixFirstItems(ids []string, byID map[string]CommentView) []FixFirstItem {
	var out []FixFirstItem
	seen := map[string]bool{}
	for _, id := range ids {
		v, ok := byID[id]
		// A label and a location can name the same finding; list it once.
		if !ok || seen[v.AnchorID] {
			continue
		}
		seen[v.AnchorID] = true
		out = append(out, FixFirstItem{AnchorID: v.AnchorID, Severity: v.SeverityLabel(), FilePath: v.FilePath, LineNumber: v.LineNumber, Title: findingTitle(v.LineComment)})
	}
	return out
}

func generateReport(in ReportInput) (string, error) {
	diffFiles := ParseDiff(in.Diff)
	commentsByFile := make(map[string]map[int][]CommentView)
	var summaries []SummaryView
	var generalComments []CommentView
	var adjacentComments []CommentView
	activeComments, records := splitRecords(in.Comments)
	totalComments := len(activeComments)

	verdict, upshot, suggestions := "", "", ""
	var structured *types.SummaryBlock
	counts := map[string]int{}
	groups := map[string]*FindingGroup{}
	for _, name := range []string{groupConfirmed, groupNeedsCheck, groupMechanical} {
		groups[name] = &FindingGroup{Name: name}
	}
	byID := map[string]CommentView{}
	anchors := map[string]string{}

	for i, comment := range activeComments {
		view := CommentView{
			LineComment: comment,
			Counter:     i + 1,
			Total:       totalComments,
		}
		view.CommentBody = payload.StripProvenanceNote(comment.CommentBody)

		if comment.FilePath == "SUMMARY" {
			// Only the first SUMMARY feeds the verdict and suggestions blocks;
			// any later one keeps its text intact so nothing is lost.
			switch {
			case len(summaries) > 0:
				summaries = append(summaries, SummaryView{CommentView: view, Prose: view.CommentBody})
			case comment.Summary != nil:
				structured = comment.Summary
				verdict = summaryVerdictText[strings.ToLower(strings.TrimSpace(structured.Verdict))]
				upshot = strings.TrimSpace(structured.Upshot)
				summaries = append(summaries, SummaryView{CommentView: view, Prose: strings.TrimSpace(structured.Notes)})
			default:
				parts := decomposeSummary(view.CommentBody)
				verdict, suggestions = parts.Verdict, parts.Suggestions
				summaries = append(summaries, SummaryView{CommentView: view, Prose: parts.Prose})
			}
			continue
		}

		group, pill, class := findingGroupFor(comment)
		counts[group]++
		view.AnchorID = fmt.Sprintf("finding-%d", view.Counter)
		view.StatusPill, view.StatusClass = pill, class
		if comment.ID != "" {
			byID[comment.ID] = view
			anchors[comment.ID] = view.AnchorID
		}
		// References fall back to file:line when the finding has no id.
		loc := comment.FilePath
		if comment.LineNumber > 0 {
			loc = fmt.Sprintf("%s:%d", comment.FilePath, comment.LineNumber)
		}
		if _, taken := anchors[loc]; !taken {
			anchors[loc] = view.AnchorID
			byID[loc] = view
		}
		row := FindingRow{
			AnchorID: view.AnchorID, Severity: view.SeverityLabel(), StatusPill: pill, StatusClass: class,
			FilePath: comment.FilePath, LineNumber: comment.LineNumber, Title: findingTitle(comment),
		}

		if comment.FilePath == "GENERAL" {
			generalComments = append(generalComments, view)
			groups[group].Rows = append(groups[group].Rows, row)
			continue
		}

		canDisplayInline := false
		for _, diffFile := range diffFiles {
			if diffFile.Path != comment.FilePath {
				continue
			}
			if comment.LineNumber == 0 {
				// Whole-file finding: displayable whenever the file is in
				// the diff. Must not match per-line, since added/deleted
				// lines carry a zero Old/NewLineNumber.
				canDisplayInline = true
				break
			}
			for _, line := range diffFile.Lines {
				if line.NewLineNumber == comment.LineNumber || line.OldLineNumber == comment.LineNumber {
					canDisplayInline = true
					break
				}
			}
			break
		}

		if canDisplayInline {
			if _, ok := commentsByFile[comment.FilePath]; !ok {
				commentsByFile[comment.FilePath] = make(map[int][]CommentView)
			}
			commentsByFile[comment.FilePath][comment.LineNumber] = append(commentsByFile[comment.FilePath][comment.LineNumber], view)
		} else {
			// Off-diff finding: shown with surrounding code when the file
			// is available, and as a bare comment otherwise. Never dropped.
			if in.FileContents != nil {
				view.ContextLines = generateContextLines(comment.FilePath, comment.LineNumber, in.FileContents)
			}
			adjacentComments = append(adjacentComments, view)
		}
		groups[group].Rows = append(groups[group].Rows, row)
	}

	var findingGroups []FindingGroup
	for _, name := range []string{groupConfirmed, groupNeedsCheck, groupMechanical} {
		if g := groups[name]; len(g.Rows) > 0 {
			findingGroups = append(findingGroups, *g)
		}
	}

	if verdict == "" {
		verdict = "Verdict: unavailable"
	}
	var actions []FixFirstItem
	if structured != nil {
		actions = fixFirst(structured.PriorityIDs, byID, counts[groupConfirmed]+counts[groupNeedsCheck]+counts[groupMechanical])
	}
	rejected := recordViews(records, stateRejected, anchors)

	shortCommitSHA := in.CommitSHA
	if len(in.CommitSHA) > 7 {
		shortCommitSHA = in.CommitSHA[:7]
	}

	alerts := buildDeterministicAlerts(in.Comments, in.Checks)
	var pinnedAlerts []AlertView
	if surfaceAlertsEnabled() {
		pinnedAlerts = alerts
	}

	description := buildDescription(in.PRBody)
	pageTitle := strings.TrimSpace(in.PRTitle)
	heading := pageTitle
	if pageTitle == "" {
		pageTitle = fmt.Sprintf("#%d", in.PRNumber)
		heading = fmt.Sprintf("PR #%d", in.PRNumber)
	}

	reportData := struct {
		PRNumber             int
		PRURL                string
		PageTitle            string
		Heading              string
		PRBody               template.HTML
		Description          descriptionView
		Verdict              string
		VerdictClass         string
		Upshot               string
		ConfirmedCount       int
		UnverifiedCount      int
		MechanicalCount      int
		RejectedCount        int
		Suggestions          string
		FixFirst             []FixFirstItem
		Rejected             []RecordView
		Unexamined           []RecordView
		Merged               []RecordView
		Prompt               template.HTML
		Files                []*DiffFile
		Comments             map[string]map[int][]CommentView
		Summaries            []SummaryView
		GeneralComments      []CommentView
		AdjacentComments     []CommentView
		FindingGroups        []FindingGroup
		PinnedAlerts         []AlertView
		DeterministicAlerts  []AlertView
		Checks               []CheckRecord
		RawDiff              string
		ModelName            string
		PromptTokenCount     int32
		CandidatesTokenCount int32
		TotalTokenCount      int32
		GeneratedAt          string
		CommitSHA            string
		ShortCommitSHA       string
	}{
		PRNumber:        in.PRNumber,
		PRURL:           in.PRURL,
		PageTitle:       pageTitle,
		Heading:         heading,
		PRBody:          description.Full,
		Description:     description,
		Verdict:         verdict,
		VerdictClass:    verdictClass(verdict),
		Upshot:          upshot,
		ConfirmedCount:  counts[groupConfirmed],
		UnverifiedCount: counts[groupNeedsCheck],
		MechanicalCount: counts[groupMechanical],
		RejectedCount:   len(rejected),
		Suggestions:     suggestions,
		FixFirst:        actions,
		Rejected:        rejected,
		Unexamined:      recordViews(records, stateUnverified, anchors),
		Merged:          recordViews(records, stateMerged, anchors),
		// The prompt embeds raw diff/code (e.g. template syntax, <script> tags,
		// jQuery). It is NOT Markdown: escape it so it renders as literal text
		// instead of being interpreted as HTML/math. CSS handles line wrapping.
		Prompt:               template.HTML(html.EscapeString(in.Prompt)),
		Files:                diffFiles,
		Comments:             commentsByFile,
		Summaries:            summaries,
		GeneralComments:      generalComments,
		AdjacentComments:     adjacentComments,
		FindingGroups:        findingGroups,
		PinnedAlerts:         pinnedAlerts,
		DeterministicAlerts:  alerts,
		Checks:               in.Checks,
		RawDiff:              in.Diff,
		ModelName:            in.ModelName,
		PromptTokenCount:     in.PromptTokens,
		CandidatesTokenCount: in.CandidateTokens,
		TotalTokenCount:      in.TotalTokens,
		GeneratedAt:          in.GeneratedAt.Format("Monday, January 2, 2006 at 3:04 PM MST"),
		CommitSHA:            in.CommitSHA,
		ShortCommitSHA:       shortCommitSHA,
	}

	funcMap := template.FuncMap{
		"markdownify": func(s string) template.HTML {
			return renderMarkdown(s)
		},
		"dict": func(values ...interface{}) (map[string]interface{}, error) {
			if len(values)%2 != 0 {
				return nil, fmt.Errorf("dict expects an even number of arguments: got %d", len(values))
			}
			m := make(map[string]interface{}, len(values)/2)
			for i := 0; i < len(values); i += 2 {
				key, ok := values[i].(string)
				if !ok {
					return nil, fmt.Errorf("dict keys must be strings: got %T", values[i])
				}
				m[key] = values[i+1]
			}
			return m, nil
		},
	}

	tmpl, err := template.New("").Funcs(funcMap).ParseFS(templateFS, "templates/*.tmpl")
	if err != nil {
		return "", fmt.Errorf("error parsing template: %w", err)
	}

	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "layout", reportData); err != nil {
		return "", fmt.Errorf("error executing template: %w", err)
	}

	return buf.String(), nil
}

// generateContextLines creates context lines around a specific line number for adjacent comments
func generateContextLines(filePath string, lineNumber int, fileContents map[string]string) []ContextLine {
	return GenerateContextLinesForTest(filePath, lineNumber, fileContents)
}

// GenerateContextLinesForTest exports the context line generation for testing
func GenerateContextLinesForTest(filePath string, lineNumber int, fileContents map[string]string) []ContextLine {
	content, exists := fileContents[filePath]
	if !exists {
		return nil
	}

	lines := strings.Split(content, "\n")
	if lineNumber <= 0 || lineNumber > len(lines) {
		return nil
	}

	// Show 3 lines before and after the target line
	contextRange := 3
	start := lineNumber - contextRange - 1 // Convert to 0-based indexing
	end := lineNumber + contextRange - 1

	if start < 0 {
		start = 0
	}
	if end >= len(lines) {
		end = len(lines) - 1
	}

	var contextLines []ContextLine
	for i := start; i <= end; i++ {
		contextLine := ContextLine{
			LineNumber: i + 1, // Convert back to 1-based
			Content:    template.HTML(html.EscapeString(lines[i])),
			IsTarget:   i+1 == lineNumber,
		}
		contextLines = append(contextLines, contextLine)
	}

	return contextLines
}
