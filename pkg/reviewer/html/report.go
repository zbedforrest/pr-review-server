package html

import (
	"bytes"
	"embed"
	"fmt"
	"html"
	"html/template"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/gomarkdown/markdown"
	mdhtml "github.com/gomarkdown/markdown/html"
	"github.com/gomarkdown/markdown/parser"
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

// MathJax is left out so a dollar sign in prose stays a dollar sign.
const mdExtensions = (parser.CommonExtensions | parser.AutoHeadingIDs) &^ parser.MathJax

// renderMarkdown converts Markdown to HTML and sanitizes the result. Use this
// for genuine Markdown fields. Do NOT use it for code or diff text: that is
// not Markdown and must be passed through html.EscapeString instead.
func renderMarkdown(s string) template.HTML {
	// Flags 0 keeps Smartypants off, so fractions, dashes and quotes stay as written.
	renderer := mdhtml.NewRenderer(mdhtml.RendererOptions{Flags: 0})
	unsafe := markdown.ToHTML([]byte(s), parser.NewWithExtensions(mdExtensions), renderer)
	return template.HTML(mdSanitizer.SanitizeBytes(unsafe))
}

// renderMarkdownInline renders s for a title or an inline slot: a lone
// wrapping paragraph is dropped so the text gains no block margins.
func renderMarkdownInline(s string) template.HTML {
	out := strings.TrimSpace(string(renderMarkdown(s)))
	if strings.HasPrefix(out, "<p>") && strings.HasSuffix(out, "</p>") && strings.Count(out, "<p>") == 1 {
		out = strings.TrimSuffix(strings.TrimPrefix(out, "<p>"), "</p>")
	}
	return template.HTML(out)
}

// lineTypeGap marks a separator row for new-side lines the diff skips. It
// carries no line numbers, so no comment can attach to it.
const lineTypeGap = "gap"

// Line represents a single line in a diff.
type Line struct {
	Type          string
	Content       template.HTML
	OldLineNumber int
	NewLineNumber int
	Truncated     bool
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
	// DisplayTitle and DisplayBody are the rewrite GitHub shows in place of
	// the original body; empty when no rewrite ran.
	DisplayTitle string
	DisplayBody  string
	Detail       FindingDetail
	// AlsoCovers titles the first-pass claims folded into this finding.
	AlsoCovers []string
	VerifyMark
}

// FindingDetail is the finding contract's context that GitHub folds under an
// inline comment.
type FindingDetail struct {
	Impact          string
	Uncertainty     string
	VerifyCondition string
	VerifyExpected  string
}

// HasDetail reports whether the contract said anything worth folding in.
func (v CommentView) HasDetail() bool {
	return v.Detail != FindingDetail{}
}

// displayOf is the rewrite GitHub posts for c, or nil when it has none.
func displayOf(c types.LineComment) *types.Display {
	if c.Display == nil || strings.TrimSpace(c.Display.Title) == "" || strings.TrimSpace(c.Display.Body) == "" {
		return nil
	}
	return c.Display
}

func findingDetail(c *types.FindingContract) FindingDetail {
	if c == nil || types.ContractStatus(c) != "valid" {
		return FindingDetail{}
	}
	d := FindingDetail{Impact: strings.TrimSpace(c.CurrentImpact), Uncertainty: strings.TrimSpace(c.Uncertainty)}
	if c.Falsifiability == "falsifiable" && c.FalsifiableCondition != nil && c.ExpectedObservable != nil {
		d.VerifyCondition = strings.TrimSuffix(strings.TrimSpace(*c.FalsifiableCondition), ".")
		d.VerifyExpected = strings.TrimSuffix(strings.TrimSpace(*c.ExpectedObservable), ".")
	}
	return d
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

// VerifyMark is how the verifier and the quality gate left a finding. It is
// separate from the index groups, which record what the reviewing agent
// itself concluded: a finding the agent confirmed can still be refuted here.
type VerifyMark struct {
	VerifyPill    string
	VerifyClass   string
	VerifyNote    string   // one-line meaning of the pill, shown on hover
	VerifyReasons []string // what the verifier and the gate said, when they said anything
}

// Verification marks, in the order the report counts them.
const (
	verifyVerified    = "VERIFIED"
	verifyRevised     = "VERIFIED · REVISED"
	verifyGradedGood  = "GRADED GOOD"
	verifyGradedOK    = "GRADED OK"
	verifyDemoted     = "DEMOTED TO NOTE"
	verifyHeld        = "HELD"
	verifyLowValue    = "LOW VALUE"
	verifyRefuted     = "REFUTED"
	verifyGradedBad   = "GRADED BAD"
	verifyUnavailable = "COULD NOT VERIFY"
	verifyNone        = "NOT VERIFIED"
)

var verifyMarkOrder = []string{verifyVerified, verifyRevised, verifyGradedGood, verifyGradedOK, verifyDemoted, verifyHeld, verifyLowValue, verifyRefuted, verifyGradedBad, verifyUnavailable, verifyNone}

// VerifyCount is one verification mark and how many findings carry it.
type VerifyCount struct {
	Label string
	Class string
	Count int
}

// verifyMarkFor labels a finding by its applied verification. A decision that
// was only recorded (shadow mode) changed nothing, so it reads as not
// verified. Whatever keeps the finding off the PR outranks a kept finding;
// a gate grade labels the finding only when the verifier did not decide.
func verifyMarkFor(v *types.Verification) VerifyMark {
	m := VerifyMark{VerifyPill: verifyNone, VerifyClass: "none", VerifyNote: "No verifier checked this finding against the repository."}
	if v == nil {
		return m
	}
	if v.Applied && strings.TrimSpace(v.Reason) != "" {
		m.VerifyReasons = append(m.VerifyReasons, "Verifier: "+strings.TrimSpace(v.Reason))
	}
	if v.Applied && v.LowValue && strings.TrimSpace(v.ValueReason) != "" {
		m.VerifyReasons = append(m.VerifyReasons, "Not worth posting: "+strings.TrimSpace(v.ValueReason))
	}
	if v.GateApplied && strings.TrimSpace(v.GateReason) != "" {
		m.VerifyReasons = append(m.VerifyReasons, fmt.Sprintf("Quality gate (%s): %s", v.Gate, strings.TrimSpace(v.GateReason)))
	}
	status := v.Status
	if !v.Applied {
		status = ""
	}
	gate := ""
	if v.GateApplied {
		gate = v.Gate
	}
	switch {
	case status == types.VerifyDrop:
		m.VerifyPill, m.VerifyClass, m.VerifyNote = verifyRefuted, "refuted", "The verifier checked the repository and found the claim wrong."
	case status == types.VerifyHold:
		m.VerifyPill, m.VerifyClass, m.VerifyNote = verifyHeld, "disputed", "The verifier could not stand behind the claim as written, so it is not posted."
	case v.Applied && v.LowValue:
		m.VerifyPill, m.VerifyClass, m.VerifyNote = verifyLowValue, "disputed", "The verifier judged this true but not worth posting."
	case gate == "BAD" && v.GateDemoted:
		m.VerifyPill, m.VerifyClass, m.VerifyNote = verifyDemoted, "disputed", "The quality gate graded this critical BAD, so it is posted as a LOW note."
	case gate == "BAD":
		m.VerifyPill, m.VerifyClass, m.VerifyNote = verifyGradedBad, "refuted", "The quality gate graded this finding BAD, so it is not posted."
	case status == types.VerifyKeep:
		m.VerifyPill, m.VerifyClass, m.VerifyNote = verifyVerified, "verified", "The verifier checked the claim against the repository and kept it."
	case status == types.VerifyRevise:
		m.VerifyPill, m.VerifyClass, m.VerifyNote = verifyRevised, "verified", "The verifier checked the claim against the repository and corrected it."
	case status == types.VerifyUnavailable:
		m.VerifyPill, m.VerifyClass, m.VerifyNote = verifyUnavailable, "unavailable", "The verifier looked but the repository could not settle the claim."
	case gate == "GOOD":
		m.VerifyPill, m.VerifyClass, m.VerifyNote = verifyGradedGood, "verified", "The quality gate graded this finding GOOD; no verifier checked it against the repository."
	case gate == "OK":
		m.VerifyPill, m.VerifyClass, m.VerifyNote = verifyGradedOK, "unavailable", "The quality gate graded this finding OK; no verifier checked it against the repository."
	}
	return m
}

// verificationRan reports whether any verifier or gate decision was applied
// to the review's findings; without one, every pill would read NOT VERIFIED.
func verificationRan(comments []types.LineComment) bool {
	for _, c := range comments {
		if v := c.Verification; v != nil && (v.Applied && v.Status != types.VerifySkipped || v.GateApplied) {
			return true
		}
	}
	return false
}

func verifyCounts(byPill map[string]VerifyMark, counts map[string]int) []VerifyCount {
	var out []VerifyCount
	for _, label := range verifyMarkOrder {
		if n := counts[label]; n > 0 {
			out = append(out, VerifyCount{Label: strings.ToLower(label), Class: byPill[label].VerifyClass, Count: n})
		}
	}
	return out
}

// Findings-index groups, in reading order.
const (
	groupInline     = "Inline comments on the PR"
	groupNotes      = "Summary notes"
	groupConfirmed  = "Confirmed"
	groupNeedsCheck = "Needs verification"
	groupMechanical = "Mechanical signals"
)

var indexGroupOrder = []string{groupInline, groupNotes, groupConfirmed, groupNeedsCheck, groupMechanical}

func severityRank(severity string) int {
	switch severityClass(severity) {
	case "critical":
		return 0
	case "medium":
		return 1
	case "low":
		return 2
	}
	return 3
}

// FindingRow is one line of the findings index.
type FindingRow struct {
	AnchorID    string
	Severity    string
	StatusPill  string
	StatusClass string
	FilePath    string
	LineNumber  int
	Title       string
	order       int
	VerifyMark
}

// SeverityClass returns the CSS-class suffix for the severity pill.
func (r FindingRow) SeverityClass() string {
	return severityClass(r.Severity)
}

// FindingGroup is one state bucket of the findings index.
type FindingGroup struct {
	Name string
	Hint string
	Rows []FindingRow
}

var groupHints = map[string]string{
	groupInline: "Clear the bar for an inline comment; past the per-PR cap they post as summary bullets instead.",
	groupNotes:  "Posted folded into the PR summary rather than inline.",
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
	// Anchor links to the finding's card when the record describes one on the page.
	Anchor string
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
	VerifyMark
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

// findingTitle is the one-line title for the findings index: the rewrite's
// title when GitHub shows one, the contract headline when set, else the first
// sentence of the body.
func findingTitle(c types.LineComment) string {
	if d := displayOf(c); d != nil {
		return truncateWords(strings.TrimSpace(d.Title), findingTitleRunes)
	}
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
	preview := truncateWords(firstProseParagraph(body), descriptionPreviewRunes)
	return descriptionView{Folded: true, Preview: renderMarkdown(preview), Full: full}
}

var (
	htmlCommentRe  = regexp.MustCompile(`(?s)<!--.*?-->`)
	headingLineRe  = regexp.MustCompile(`^\s*#{1,6}(\s|$)`)
	emptyListingRe = regexp.MustCompile(`^[\s*_-]*(none|n/a)?[\s.*_-]*$`)
)

// firstProseParagraph is the first paragraph of a PR body that says
// something: template headings, HTML comments and empty answers are skipped.
func firstProseParagraph(body string) string {
	for _, p := range splitParagraphs(htmlCommentRe.ReplaceAllString(body, "")) {
		var kept []string
		for _, line := range strings.Split(p, "\n") {
			if !headingLineRe.MatchString(line) {
				kept = append(kept, line)
			}
		}
		if text := strings.TrimSpace(strings.Join(kept, "\n")); !emptyListingRe.MatchString(strings.ToLower(text)) {
			return text
		}
	}
	return ""
}

// ContextLine represents a line of code context for adjacent comments
type ContextLine struct {
	LineNumber int
	Content    template.HTML
	IsTarget   bool // True if this is the line being commented on
}

var hunkHeaderRe = regexp.MustCompile(`^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@`)

// ParseDiff parses a raw unified diff into files of numbered lines. Each hunk
// that starts past the previous one is preceded by a gap row.
func ParseDiff(diff string) []*DiffFile {
	var files []*DiffFile
	lines := strings.Split(diff, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	var currentFile *DiffFile
	var oldLine, newLine int
	var oldPath string
	inHunk := false

	for _, line := range lines {
		if strings.HasPrefix(line, "diff --git ") {
			currentFile = &DiffFile{Path: diffGitPath(strings.TrimPrefix(line, "diff --git "))}
			files = append(files, currentFile)
			oldLine, newLine, oldPath, inHunk = 1, 1, "", false
			continue
		}
		if currentFile == nil {
			continue
		}

		if strings.HasPrefix(line, "@@") {
			oldStart, newStart, ok := parseHunkHeader(line)
			inHunk = ok
			if !ok {
				continue
			}
			if gap := newStart - newLine; gap > 0 {
				currentFile.Lines = append(currentFile.Lines, Line{
					Type:    lineTypeGap,
					Content: template.HTML(gapLabel(gap)),
				})
			}
			oldLine, newLine = oldStart, newStart
			continue
		}

		if !inHunk {
			switch {
			case strings.HasPrefix(line, "--- "):
				oldPath = diffSidePath(strings.TrimPrefix(line, "--- "), "a/")
			case strings.HasPrefix(line, "+++ "):
				if path := diffSidePath(strings.TrimPrefix(line, "+++ "), "b/"); path != "" {
					currentFile.Path = path
				} else if oldPath != "" {
					currentFile.Path = oldPath
				}
			case strings.HasPrefix(line, "rename to "):
				currentFile.Path = unquoteGitPath(strings.TrimPrefix(line, "rename to "))
			case strings.HasPrefix(line, "copy to "):
				currentFile.Path = unquoteGitPath(strings.TrimPrefix(line, "copy to "))
			}
			continue
		}

		switch {
		case strings.HasPrefix(line, "\\"):
		case strings.HasPrefix(line, "+"):
			currentFile.Lines = append(currentFile.Lines, diffLine("added", strings.TrimPrefix(line, "+"), 0, newLine))
			newLine++
		case strings.HasPrefix(line, "-"):
			currentFile.Lines = append(currentFile.Lines, diffLine("deleted", strings.TrimPrefix(line, "-"), oldLine, 0))
			oldLine++
		default:
			currentFile.Lines = append(currentFile.Lines, diffLine("context", strings.TrimPrefix(line, " "), oldLine, newLine))
			oldLine++
			newLine++
		}
	}

	return files
}

// Page weight limits: the longest diff line shown, and how much line content
// uncommented files may add to one page.
const (
	maxLineBytes    = 2000
	diffBudgetBytes = 400 * 1024
)

// diffLine builds a diff row, cutting an overlong line before it is escaped
// so the cut never lands inside an HTML entity.
func diffLine(kind, text string, oldLine, newLine int) Line {
	text, truncated := truncateLine(text)
	return Line{
		Type:          kind,
		Content:       template.HTML(html.EscapeString(text)),
		OldLineNumber: oldLine,
		NewLineNumber: newLine,
		Truncated:     truncated,
	}
}

// truncateLine cuts text to at most maxLineBytes without splitting a rune.
func truncateLine(text string) (string, bool) {
	if len(text) <= maxLineBytes {
		return text, false
	}
	cut := maxLineBytes
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut], true
}

const (
	fileOpen      = "open"
	fileCollapsed = "collapsed"
	fileSkipped   = "skipped"
)

// FileView is a diff file with how the page shows it.
type FileView struct {
	*DiffFile
	Mode      string
	LineCount int
}

func (f FileView) Collapsed() bool { return f.Mode == fileCollapsed }

func (f FileView) Skipped() bool { return f.Mode == fileSkipped }

// LineLabel is the file's size in diff lines, gap rows excluded.
func (f FileView) LineLabel() string {
	if f.LineCount == 1 {
		return "1 line"
	}
	return fmt.Sprintf("%d lines", f.LineCount)
}

var lockfileNames = map[string]bool{
	"package-lock.json": true, "yarn.lock": true, "pnpm-lock.yaml": true, "go.sum": true, "Cargo.lock": true,
	"poetry.lock": true, "Pipfile.lock": true, "composer.lock": true, "Gemfile.lock": true,
}

var generatedDirs = map[string]bool{"vendor": true, "node_modules": true, "dist": true, "build": true}

var generatedSuffixes = []string{".min.js", ".min.css", ".map", ".ipynb", ".pb.go", "_pb2.py", ".snap", ".svg"}

// isGeneratedPath reports whether a path names a lockfile, a vendored or
// built tree, or another file nobody reviews line by line.
func isGeneratedPath(path string) bool {
	segments := strings.Split(path, "/")
	name := segments[len(segments)-1]
	if lockfileNames[name] {
		return true
	}
	for _, dir := range segments[:len(segments)-1] {
		if generatedDirs[dir] {
			return true
		}
	}
	for _, suffix := range generatedSuffixes {
		if strings.HasSuffix(name, suffix) {
			return true
		}
	}
	return false
}

// planFiles decides how each diff file is shown, in diff order. A file with
// comments is always open and whole. Of the rest, generated files and those
// that no longer fit the budget are listed without their diff, and the others
// are collapsed. Commented files are charged to the budget first.
func planFiles(files []*DiffFile, comments map[string]map[int][]CommentView, budget int) []FileView {
	views := make([]FileView, len(files))
	sizes := make([]int, len(files))
	for i, f := range files {
		views[i] = FileView{DiffFile: f, Mode: fileCollapsed}
		for _, line := range f.Lines {
			if line.Type == lineTypeGap {
				continue
			}
			views[i].LineCount++
			sizes[i] += len(line.Content)
			if line.Truncated {
				views[i].Mode = fileSkipped
			}
		}
		switch {
		case len(comments[f.Path]) > 0:
			views[i].Mode = fileOpen
			budget -= sizes[i]
		case isGeneratedPath(f.Path):
			views[i].Mode = fileSkipped
		}
	}
	for i := range views {
		if views[i].Mode != fileCollapsed {
			continue
		}
		if sizes[i] > budget {
			views[i].Mode = fileSkipped
			continue
		}
		budget -= sizes[i]
	}
	return views
}

// parseHunkHeader returns the first old and new line a hunk's rows carry.
func parseHunkHeader(line string) (oldStart, newStart int, ok bool) {
	m := hunkHeaderRe.FindStringSubmatch(line)
	if m == nil {
		return 0, 0, false
	}
	oldStart, errOld := strconv.Atoi(m[1])
	newStart, errNew := strconv.Atoi(m[3])
	if errOld != nil || errNew != nil {
		return 0, 0, false
	}
	// A zero count names the line before the change, not a line in the hunk.
	if m[2] == "0" {
		oldStart++
	}
	if m[4] == "0" {
		newStart++
	}
	return oldStart, newStart, true
}

func gapLabel(n int) string {
	if n == 1 {
		return "1 line not shown"
	}
	return fmt.Sprintf("%d lines not shown", n)
}

// diffSidePath reads the path from a "---" or "+++" line, or "" for /dev/null.
func diffSidePath(raw, prefix string) string {
	if !strings.HasPrefix(raw, `"`) {
		raw, _, _ = strings.Cut(raw, "\t")
	}
	path := unquoteGitPath(raw)
	if path == "/dev/null" {
		return ""
	}
	return strings.TrimPrefix(path, prefix)
}

// unquoteGitPath undoes git's C-style quoting of unusual paths.
func unquoteGitPath(raw string) string {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, `"`) {
		if unquoted, err := strconv.Unquote(raw); err == nil {
			return unquoted
		}
	}
	return raw
}

// diffGitPath reads the path from the rest of a "diff --git" line, for files
// with no "+++" line (binary or mode-only changes).
func diffGitPath(rest string) string {
	// Unrenamed, both halves name the same path, which splits one with spaces.
	if mid := len(rest) / 2; len(rest)%2 == 1 && rest[mid] == ' ' {
		oldSide := strings.TrimPrefix(unquoteGitPath(rest[:mid]), "a/")
		newSide := strings.TrimPrefix(unquoteGitPath(rest[mid+1:]), "b/")
		if oldSide == newSide {
			return newSide
		}
	}
	for _, sep := range []string{` "b/`, " b/"} {
		if i := strings.LastIndex(rest, sep); i >= 0 {
			return strings.TrimPrefix(unquoteGitPath(rest[i+1:]), "b/")
		}
	}
	if i := strings.LastIndex(rest, " "); i >= 0 {
		return unquoteGitPath(rest[i+1:])
	}
	return unquoteGitPath(rest)
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

// StageRecord is one pipeline stage in the generation block: what ran, on
// which model, and what it cost.
type StageRecord struct {
	Name       string
	Model      string
	Detail     string
	CostUSD    float64
	DurationMS int64
}

// Cost is the stage's spend for display, "" when none was reported.
func (s StageRecord) Cost() string {
	if s.CostUSD <= 0 {
		return ""
	}
	return fmt.Sprintf("$%.2f", s.CostUSD)
}

// Duration is the stage's wall time for display, "" when unknown.
func (s StageRecord) Duration() string {
	if s.DurationMS <= 0 {
		return ""
	}
	return (time.Duration(s.DurationMS) * time.Millisecond).Round(time.Second).String()
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
	CommitSHA       string
	ModelName       string
	PromptTokens    int32
	CandidateTokens int32
	TotalTokens     int32
	GeneratedAt     time.Time
	FileContents    map[string]string
	Checks          []CheckRecord
	// Profile names the review flavor for the header ("Full", "Lite",
	// "Custom (based on Lite)"); ProfileDeviations lists the fields a custom
	// run changed from that profile. Empty Profile renders no profile line.
	Profile           string
	ProfileDeviations []string
	// Stages lists the pipeline stages that ran; empty renders the single
	// model and token counts instead.
	Stages  []StageRecord
	Publish PublishView
}

// PublishView is what the publisher will make of the review, worked out
// before it posts. Withhold reports whether it keeps a finding off the PR;
// such findings are left out of the page entirely and only counted (nil
// keeps all). Confidence is the merge confidence the PR summary reports,
// 0 when unknown.
// Inline reports whether a finding clears the bar for an inline comment, so
// the index can split what posts inline from what folds into the summary.
// Title is the line GitHub lists a finding by, so the index reads the same;
// nil or "" falls back to the report's own title. NotPublished says why the
// review did not post ("the PR is a draft"), "" when it posted or will.
type PublishView struct {
	Withhold     func(types.LineComment) bool
	Inline       func(types.LineComment) bool
	Confidence   int
	Title        func(types.LineComment) string
	NotPublished string
}

// known reports whether the view says what reached the PR; without it the
// page is the reviewer's own account and keeps the reviewer's summary.
func (v PublishView) known() bool {
	return v.NotPublished != "" || v.Withhold != nil
}

func (v PublishView) titleOf(c types.LineComment) string {
	if v.Title != nil {
		if t := strings.TrimSpace(v.Title(c)); t != "" {
			return truncateWords(t, findingTitleRunes)
		}
	}
	return findingTitle(c)
}

func plural(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// postedLine states what reached the PR, from the publisher's own counts.
func postedLine(v PublishView, inline, notes int, shortSHA string) string {
	if !v.known() {
		return ""
	}
	if v.NotPublished != "" {
		return "Not posted to the PR: " + strings.TrimSuffix(v.NotPublished, ".") + "."
	}
	if v.Inline == nil && notes > 0 {
		return "Posted " + plural(notes, "comment") + "."
	}
	var parts []string
	if inline > 0 {
		parts = append(parts, plural(inline, "inline comment"))
	}
	if notes > 0 {
		parts = append(parts, plural(notes, "summary note"))
	}
	if len(parts) == 0 {
		return "No comments posted on " + shortSHA + "."
	}
	return "Posted " + strings.Join(parts, " and ") + "."
}

const topFindingsShown = 3

// topFindings is the short list under the posted line: the most severe
// posted findings, dropped when it would repeat the whole index.
func topFindings(posted []FixFirstItem) []FixFirstItem {
	if len(posted) <= topFindingsShown {
		return nil
	}
	top := append([]FixFirstItem(nil), posted...)
	sort.SliceStable(top, func(i, j int) bool { return severityRank(top[i].Severity) < severityRank(top[j].Severity) })
	return top[:topFindingsShown]
}

// idRefs rewrites the reviewer's internal finding labels, which mean nothing
// to a reader: a label of a finding on the page becomes its location, any
// other known label becomes "another finding".
type idRefs struct {
	re      *regexp.Regexp
	anchors map[string]string
	locs    map[string]string
}

const maxInternalIDLen = 12

func newIDRefs(comments []types.LineComment) idRefs {
	seen := map[string]bool{}
	var ids []string
	for _, c := range comments {
		id := strings.TrimSpace(c.ID)
		if id == "" || len(id) > maxInternalIDLen || !strings.ContainsAny(id, "0123456789") || seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, regexp.QuoteMeta(id))
	}
	if len(ids) == 0 {
		return idRefs{}
	}
	sort.Slice(ids, func(i, j int) bool { return len(ids[i]) > len(ids[j]) })
	return idRefs{re: regexp.MustCompile(strings.Join(ids, "|")), anchors: map[string]string{}, locs: map[string]string{}}
}

func (r idRefs) show(id, anchor, loc string) {
	if r.re != nil && id != "" {
		r.anchors[id], r.locs[id] = anchor, loc
	}
}

func idChar(b byte) bool {
	return b == '-' || b == '_' || b == '/' || b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z'
}

func (r idRefs) replace(s string, render func(id string) string) string {
	if r.re == nil {
		return s
	}
	var b strings.Builder
	last := 0
	for _, m := range r.re.FindAllStringIndex(s, -1) {
		if m[0] > 0 && idChar(s[m[0]-1]) || m[1] < len(s) && idChar(s[m[1]]) {
			continue
		}
		b.WriteString(s[last:m[0]])
		b.WriteString(render(s[m[0]:m[1]]))
		last = m[1]
	}
	b.WriteString(s[last:])
	return b.String()
}

const anotherFinding = "another finding"

func (r idRefs) plain(s string) string {
	return r.replace(s, func(id string) string {
		if loc := r.locs[id]; loc != "" {
			return loc
		}
		return anotherFinding
	})
}

// markdown links shown labels to their cards and leaves code alone.
func (r idRefs) markdown(s string) string {
	if r.re == nil {
		return s
	}
	return outsideCode(s, func(prose string) string {
		return r.replace(prose, func(id string) string {
			if loc := r.locs[id]; loc != "" {
				return fmt.Sprintf("[`%s`](#%s)", loc, r.anchors[id])
			}
			return anotherFinding
		})
	})
}

// outsideCode applies fn to the text outside backtick spans and fences.
func outsideCode(s string, fn func(string) string) string {
	var b strings.Builder
	for len(s) > 0 {
		i := strings.IndexByte(s, '`')
		if i < 0 {
			b.WriteString(fn(s))
			break
		}
		b.WriteString(fn(s[:i]))
		j := i
		for j < len(s) && s[j] == '`' {
			j++
		}
		end := strings.Index(s[j:], s[i:j])
		if end < 0 {
			b.WriteString(s[i:])
			break
		}
		stop := j + end + (j - i)
		b.WriteString(s[i:stop])
		s = s[stop:]
	}
	return b.String()
}

// GenerateReportFrom renders the review report from a ReportInput.
func GenerateReportFrom(in ReportInput) (string, error) {
	return generateReport(in)
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

// withheldOut drops the active findings the publisher keeps off the PR and
// returns how many it dropped. Summaries and inactive records are kept.
func withheldOut(comments []types.LineComment, withhold func(types.LineComment) bool) ([]types.LineComment, int) {
	if withhold == nil {
		return comments, 0
	}
	kept := make([]types.LineComment, 0, len(comments))
	n := 0
	for _, c := range comments {
		if !c.Inactive && c.FilePath != "SUMMARY" && withhold(c) {
			n++
			continue
		}
		kept = append(kept, c)
	}
	return kept, n
}

// confidenceClass colors the verdict block by merge confidence the way the
// summary's score reads: 4 or 5 is mergeable, 1 or 2 blocks.
func confidenceClass(confidence int) string {
	switch {
	case confidence >= 4:
		return "approve"
	case confidence <= 2:
		return "changes"
	}
	return "neutral"
}

func stagesCost(stages []StageRecord) string {
	total := 0.0
	for _, s := range stages {
		total += s.CostUSD
	}
	return StageRecord{CostUSD: total}.Cost()
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
func recordViews(records []types.LineComment, state string, refs idRefs) []RecordView {
	var out []RecordView
	for _, c := range records {
		if c.State != state {
			continue
		}
		v := RecordView{Severity: severityLabel(c.Importance), FilePath: c.FilePath, LineNumber: c.LineNumber,
			Claim: payload.StripProvenanceNote(c.CommentBody)}
		if c.Original != nil && strings.TrimSpace(c.Original.Comment) != "" {
			v.Claim = c.Original.Comment
		}
		v.Claim = refs.markdown(v.Claim)
		if c.Assessment != nil {
			v.Reason = refs.plain(strings.TrimSpace(c.Assessment.Reason))
			for _, e := range c.Assessment.Evidence {
				v.Evidence = append(v.Evidence, location(e.File, e.Line))
			}
		}
		out = append(out, v)
	}
	return out
}

// alsoCovers titles the merged records by the finding that absorbed them,
// keyed the way MergedInto names it: an id or a file:line.
func alsoCovers(records []types.LineComment, title func(types.LineComment) string) map[string][]string {
	out := map[string][]string{}
	for _, c := range records {
		if c.State != stateMerged || c.MergedInto == "" {
			continue
		}
		if t := title(c); t != "" {
			out[c.MergedInto] = append(out[c.MergedInto], t)
		}
	}
	return out
}

// fixFirst resolves the structured SUMMARY's priority ids against the
// findings on the page; ids that name nothing rendered are skipped. The list
// is the reviewer's short list, so it is dropped when it would repeat the full
// findings index rather than shorten it.
func fixFirst(ids []string, byID map[string]CommentView, totalFindings int, title func(types.LineComment) string) []FixFirstItem {
	actions := fixFirstItems(ids, byID, title)
	if len(actions) >= totalFindings {
		return nil
	}
	return actions
}

func fixFirstItems(ids []string, byID map[string]CommentView, title func(types.LineComment) string) []FixFirstItem {
	var out []FixFirstItem
	seen := map[string]bool{}
	for _, id := range ids {
		v, ok := byID[id]
		// A label and a location can name the same finding; list it once.
		if !ok || seen[v.AnchorID] {
			continue
		}
		seen[v.AnchorID] = true
		out = append(out, fixFirstItem(v, title(v.LineComment)))
	}
	return out
}

// Page sections in the order the layout renders their cards.
const (
	sectionNone = iota
	sectionSummary
	sectionGeneral
	sectionDiff
	sectionAdjacent
)

// cardPlace is where one active comment's card lands on the page.
type cardPlace struct {
	index, section, file, line int
}

// diffPlace finds the diff file a finding renders under: whole-file findings
// under the header, the rest under a new-side line (a deleted line carries
// no new-side number, so a finding there falls to the adjacent section).
func diffPlace(files []*DiffFile, c types.LineComment) (int, bool) {
	for i, f := range files {
		if f.Path != c.FilePath {
			continue
		}
		if c.LineNumber == 0 {
			return i, true
		}
		for _, line := range f.Lines {
			if line.NewLineNumber == c.LineNumber {
				return i, true
			}
		}
		return 0, false
	}
	return 0, false
}

// numberCards numbers the cards in page order; a place with no card gets 0.
func numberCards(places []cardPlace) (counters []int, total int) {
	ordered := make([]cardPlace, 0, len(places))
	for _, p := range places {
		if p.section != sectionNone {
			ordered = append(ordered, p)
		}
	}
	sort.SliceStable(ordered, func(i, j int) bool {
		a, b := ordered[i], ordered[j]
		if a.section != b.section {
			return a.section < b.section
		}
		if a.section != sectionDiff {
			return false
		}
		if a.file != b.file {
			return a.file < b.file
		}
		return a.line < b.line
	})
	counters = make([]int, len(places))
	for n, p := range ordered {
		counters[p.index] = n + 1
	}
	return counters, len(ordered)
}

func fixFirstItem(v CommentView, title string) FixFirstItem {
	return FixFirstItem{AnchorID: v.AnchorID, Severity: v.SeverityLabel(), FilePath: v.FilePath, LineNumber: v.LineNumber, Title: title, VerifyMark: v.VerifyMark}
}

func generateReport(in ReportInput) (string, error) {
	diffFiles := ParseDiff(in.Diff)
	commentsByFile := make(map[string]map[int][]CommentView)
	var summaries []SummaryView
	var generalComments []CommentView
	var adjacentComments []CommentView
	shown, withheld := withheldOut(in.Comments, in.Publish.Withhold)
	activeComments, records := splitRecords(shown)

	verdict, upshot, suggestions := "", "", ""
	var structured *types.SummaryBlock
	counts := map[string]int{}
	groups := map[string]*FindingGroup{}
	for _, name := range indexGroupOrder {
		groups[name] = &FindingGroup{Name: name, Hint: groupHints[name]}
	}
	byID := map[string]CommentView{}
	anchors := map[string]string{}
	refs := newIDRefs(in.Comments)
	verifyTally, verifyMarks := map[string]int{}, map[string]VerifyMark{}
	showVerification := verificationRan(activeComments)
	published := in.Publish.known()
	splitByInline := in.Publish.Inline != nil && in.Publish.NotPublished == ""

	// Cards are numbered before any is built so a body can link to a later
	// one, and in the order the page lays them out so the index, the deep
	// links and the J/K order agree. A SUMMARY with nothing to say takes no
	// card and no number.
	summaryProse := map[int]string{}
	places := make([]cardPlace, len(activeComments))
	sawSummary := false
	for i, comment := range activeComments {
		places[i] = cardPlace{index: i, section: sectionAdjacent}
		switch {
		case comment.FilePath == "SUMMARY":
			prose := ""
			switch {
			case sawSummary:
				prose = payload.StripProvenanceNote(comment.CommentBody)
			case comment.Summary != nil:
				structured = comment.Summary
				verdict = summaryVerdictText[strings.ToLower(strings.TrimSpace(structured.Verdict))]
				upshot = strings.TrimSpace(structured.Upshot)
				prose = strings.TrimSpace(structured.Notes)
			default:
				parts := decomposeSummary(payload.StripProvenanceNote(comment.CommentBody))
				verdict, suggestions = parts.Verdict, parts.Suggestions
				prose = parts.Prose
			}
			sawSummary = true
			places[i].section = sectionSummary
			if strings.TrimSpace(prose) == "" {
				places[i].section = sectionNone
				continue
			}
			summaryProse[i] = prose
		case comment.FilePath == "GENERAL":
			places[i].section = sectionGeneral
		default:
			if file, ok := diffPlace(diffFiles, comment); ok {
				places[i].section, places[i].file, places[i].line = sectionDiff, file, comment.LineNumber
			}
		}
	}
	counters, totalComments := numberCards(places)
	for i, comment := range activeComments {
		if counters[i] == 0 || comment.FilePath == "SUMMARY" {
			continue
		}
		anchor, loc := fmt.Sprintf("finding-%d", counters[i]), location(comment.FilePath, comment.LineNumber)
		if comment.ID != "" {
			anchors[comment.ID] = anchor
			refs.show(comment.ID, anchor, loc)
		}
		// References fall back to file:line when the finding has no id.
		if _, taken := anchors[loc]; !taken {
			anchors[loc] = anchor
		}
	}
	covers := alsoCovers(records, in.Publish.titleOf)
	var posted []FixFirstItem
	var disputed []RecordView
	inlinePosted, notesPosted := 0, 0

	for i, comment := range activeComments {
		if counters[i] == 0 {
			continue
		}
		view := CommentView{
			LineComment: comment,
			Counter:     counters[i],
			Total:       totalComments,
		}
		view.CommentBody = refs.markdown(payload.StripProvenanceNote(comment.CommentBody))
		if d := displayOf(comment); d != nil {
			view.DisplayTitle, view.DisplayBody = strings.TrimSpace(d.Title), refs.markdown(strings.TrimSpace(d.Body))
		}
		view.Detail = findingDetail(comment.FindingContract)

		if comment.FilePath == "SUMMARY" {
			summaries = append(summaries, SummaryView{CommentView: view, Prose: refs.markdown(summaryProse[i])})
			continue
		}

		group, pill, class := findingGroupFor(comment)
		counts[group]++
		indexGroup := group
		if group == groupConfirmed && splitByInline {
			indexGroup = groupNotes
			if in.Publish.Inline(comment) {
				indexGroup = groupInline
			}
		}
		view.AnchorID = fmt.Sprintf("finding-%d", view.Counter)
		view.StatusPill, view.StatusClass = pill, class
		if showVerification {
			view.VerifyMark = verifyMarkFor(comment.Verification)
			for k, reason := range view.VerifyReasons {
				view.VerifyReasons[k] = refs.plain(reason)
			}
			verifyTally[view.VerifyPill]++
			verifyMarks[view.VerifyPill] = view.VerifyMark
		}
		loc := location(comment.FilePath, comment.LineNumber)
		view.AlsoCovers = covers[comment.ID]
		if comment.ID != loc && anchors[loc] == view.AnchorID {
			view.AlsoCovers = append(view.AlsoCovers[:len(view.AlsoCovers):len(view.AlsoCovers)], covers[loc]...)
		}
		if comment.ID != "" {
			byID[comment.ID] = view
		}
		if anchors[loc] == view.AnchorID {
			byID[loc] = view
		}
		title := in.Publish.titleOf(comment)
		row := FindingRow{
			AnchorID: view.AnchorID, Severity: view.SeverityLabel(), StatusPill: pill, StatusClass: class,
			FilePath: comment.FilePath, LineNumber: comment.LineNumber, Title: title, order: view.Counter, VerifyMark: view.VerifyMark,
		}
		switch {
		case indexGroup == groupInline:
			inlinePosted++
			posted = append(posted, fixFirstItem(view, title))
		case group != groupMechanical:
			notesPosted++
			posted = append(posted, fixFirstItem(view, title))
		}
		if a := comment.Assessment; a != nil {
			d := RecordView{Severity: view.SeverityLabel(), FilePath: comment.FilePath, LineNumber: comment.LineNumber, Anchor: view.AnchorID, Reason: refs.plain(strings.TrimSpace(a.Reason))}
			for _, e := range a.Evidence {
				d.Evidence = append(d.Evidence, location(e.File, e.Line))
			}
			disputed = append(disputed, d)
		}

		if comment.FilePath == "GENERAL" {
			generalComments = append(generalComments, view)
			groups[indexGroup].Rows = append(groups[indexGroup].Rows, row)
			continue
		}

		if places[i].section == sectionDiff {
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
		groups[indexGroup].Rows = append(groups[indexGroup].Rows, row)
	}

	var findingGroups []FindingGroup
	for _, name := range indexGroupOrder {
		if g := groups[name]; len(g.Rows) > 0 {
			sort.SliceStable(g.Rows, func(i, j int) bool { return g.Rows[i].order < g.Rows[j].order })
			findingGroups = append(findingGroups, *g)
		}
	}

	if verdict == "" {
		verdict = "Verdict: unavailable"
	}
	shortCommitSHA := in.CommitSHA
	if len(in.CommitSHA) > 7 {
		shortCommitSHA = in.CommitSHA[:7]
	}

	headline, headlineClass := verdict, verdictClass(verdict)
	if c := in.Publish.Confidence; c > 0 {
		headline, headlineClass = fmt.Sprintf("Merge confidence %d/5", c), confidenceClass(c)
	}
	// The reviewer wrote its upshot and priorities before anything was held
	// back, so a page that knows what posted states that instead.
	var actions, top []FixFirstItem
	switch {
	case published:
		upshot = ""
		if in.Publish.NotPublished == "" {
			top = topFindings(posted)
		}
	case structured != nil:
		actions = fixFirst(structured.PriorityIDs, byID, counts[groupConfirmed]+counts[groupNeedsCheck]+counts[groupMechanical], in.Publish.titleOf)
	}
	rejected := recordViews(records, stateRejected, refs)

	alerts := buildDeterministicAlerts(shown, in.Checks)
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
		Description          descriptionView
		Verdict              string
		VerdictClass         string
		Upshot               string
		PostedLine           string
		TopFindings          []FixFirstItem
		ConfirmedCount       int
		UnverifiedCount      int
		MechanicalCount      int
		ShownCount           int
		RejectedCount        int
		WithheldCount        int
		VerifyCounts         []VerifyCount
		Suggestions          string
		FixFirst             []FixFirstItem
		Rejected             []RecordView
		Unexamined           []RecordView
		Disputed             []RecordView
		Files                []FileView
		Comments             map[string]map[int][]CommentView
		Summaries            []SummaryView
		GeneralComments      []CommentView
		AdjacentComments     []CommentView
		FindingGroups        []FindingGroup
		PinnedAlerts         []AlertView
		DeterministicAlerts  []AlertView
		Checks               []CheckRecord
		ModelName            string
		PromptTokenCount     int32
		CandidatesTokenCount int32
		TotalTokenCount      int32
		GeneratedAt          string
		CommitSHA            string
		ShortCommitSHA       string
		Profile              string
		ProfileDeviations    string
		Stages               []StageRecord
		StagesCost           string
	}{
		PRNumber:             in.PRNumber,
		PRURL:                in.PRURL,
		PageTitle:            pageTitle,
		Heading:              heading,
		Description:          description,
		Verdict:              headline,
		VerdictClass:         headlineClass,
		Upshot:               upshot,
		PostedLine:           postedLine(in.Publish, inlinePosted, notesPosted, shortCommitSHA),
		TopFindings:          top,
		ConfirmedCount:       counts[groupConfirmed],
		UnverifiedCount:      counts[groupNeedsCheck],
		MechanicalCount:      counts[groupMechanical],
		ShownCount:           counts[groupConfirmed] + counts[groupNeedsCheck] + counts[groupMechanical],
		RejectedCount:        len(rejected),
		WithheldCount:        withheld,
		VerifyCounts:         verifyCounts(verifyMarks, verifyTally),
		Suggestions:          refs.markdown(suggestions),
		FixFirst:             actions,
		Rejected:             rejected,
		Unexamined:           recordViews(records, stateUnverified, refs),
		Disputed:             disputed,
		Files:                planFiles(diffFiles, commentsByFile, diffBudgetBytes),
		Comments:             commentsByFile,
		Summaries:            summaries,
		GeneralComments:      generalComments,
		AdjacentComments:     adjacentComments,
		FindingGroups:        findingGroups,
		PinnedAlerts:         pinnedAlerts,
		DeterministicAlerts:  alerts,
		Checks:               in.Checks,
		ModelName:            in.ModelName,
		PromptTokenCount:     in.PromptTokens,
		CandidatesTokenCount: in.CandidateTokens,
		TotalTokenCount:      in.TotalTokens,
		GeneratedAt:          in.GeneratedAt.Format("Monday, January 2, 2006 at 3:04 PM MST"),
		CommitSHA:            in.CommitSHA,
		ShortCommitSHA:       shortCommitSHA,
		Profile:              in.Profile,
		ProfileDeviations:    strings.Join(in.ProfileDeviations, ", "),
		Stages:               in.Stages,
		StagesCost:           stagesCost(in.Stages),
	}

	funcMap := template.FuncMap{
		"markdownify":    renderMarkdown,
		"markdownInline": renderMarkdownInline,
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

// generateContextLines returns the lines around lineNumber for an off-diff finding.
func generateContextLines(filePath string, lineNumber int, fileContents map[string]string) []ContextLine {
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
