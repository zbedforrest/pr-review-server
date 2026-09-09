package publisher

import (
	"fmt"
	"net/url"
	"regexp"
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
	// BadgeBaseURL serves the severity badge SVGs; empty falls back to text.
	BadgeBaseURL string
	// InlineComments maps finding id to the GitHub review-comment id it was
	// posted as (this round or earlier), so the summary can link to it.
	InlineComments map[string]int64
	// ShowUnverified is Policy.ShowUnverified as applied to this round.
	ShowUnverified bool
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
		if Shown(f) {
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
	for _, f := range r.activeClaims() {
		present[f.ID] = true
	}
	shown := map[string]bool{}
	for _, f := range append(append(r.currentFindings(), r.lowerSeverityNotes()...), r.unverifiedNotes()...) {
		shown[f.ID] = true
	}
	published := map[string]bool{}
	var d roundDiff
	for _, p := range r.Previous {
		if (p.Kind != db.PublishedKindFinding && p.Kind != db.PublishedKindAnnotation) || p.State != db.PublishedStateOpen {
			continue
		}
		published[p.Fingerprint] = true
		// Presence keeps a hidden claim from reading as fixed; the visible
		// "still open" count covers only what the comment shows.
		switch {
		case shown[p.Fingerprint]:
			d.StillOpen++
		case present[p.Fingerprint]:
		default:
			d.Fixed++
		}
	}
	// "New" counts findings the ledger tracks (the shown ones). Folded notes
	// have no ledger rows, so counting them would announce them as new on
	// every round; they still count as present so a finding that moved into a
	// fold is not reported fixed.
	for _, f := range r.currentFindings() {
		if !published[f.ID] {
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

func (r Round) findingLink(f payload.Finding) string {
	if id, ok := r.InlineComments[f.ID]; ok && id != 0 {
		return fmt.Sprintf("https://github.com/%s/%s/pull/%d#discussion_r%d", r.Owner, r.Repo, r.Number, id)
	}
	link := fmt.Sprintf("https://github.com/%s/%s/blob/%s/%s", r.Owner, r.Repo, r.HeadSHA, f.File)
	if f.Line > 0 {
		link += fmt.Sprintf("#L%d", f.Line)
	}
	return link
}

// bullet is one summary line: severity, the effect sentence, and a short
// location linked to the inline comment or the file at the reviewed commit.
func (r Round) bullet(f payload.Finding) string {
	return r.markedBullet(f, "")
}

// markedBullet is a bullet with a bold status marker between the severity and
// the text; an empty marker renders a plain bullet.
func (r Round) markedBullet(f payload.Finding, marker string) string {
	where := f.File[strings.LastIndex(f.File, "/")+1:]
	if f.Line > 0 {
		where = fmt.Sprintf("%s:%d", where, f.Line)
	}
	if marker != "" {
		marker = "**" + marker + "** "
	}
	text := truncateWords(strings.TrimSuffix(strings.TrimSpace(summaryText(f)), "."), 200)
	return fmt.Sprintf("- %s %s%s — [`%s`](%s)\n", severityLabel(f.Severity, r.BadgeBaseURL), marker, text, where, r.findingLink(f))
}

const (
	markerUnverified = "FIRST PASS · UNVERIFIED"
	markerDisputed   = "FIRST PASS · DISPUTED"
	markerCarried    = "CARRIED · UNVERIFIED"
	maxReasonRunes   = 200
)

// unverifiedBullet marks an unverified claim by where it came from (the first
// pass, or a prior round of this review), or as disputed with the agent's
// bounded reason on a nested line when it argued against the claim.
func (r Round) unverifiedBullet(f payload.Finding) string {
	if f.Assessment == nil {
		if f.Provenance == "carried" {
			return r.markedBullet(f, markerCarried)
		}
		return r.markedBullet(f, markerUnverified)
	}
	line := r.markedBullet(f, markerDisputed)
	if reason := strings.TrimSpace(f.Assessment.Reason); reason != "" {
		line += "  - Agent: " + truncateWords(firstLine(reason), maxReasonRunes) + "\n"
	}
	return line
}

// activeClaims are the findings the review holds this round: asserted as
// bullets, folded as lower-severity notes, or folded as unverified. Presence
// tracking counts all of them so a claim that moved between sections is
// neither "fixed" nor resolved.
func (r Round) activeClaims() []payload.Finding {
	var out []payload.Finding
	for _, f := range r.Findings {
		if Publishable(f) || UnverifiedNote(f) {
			out = append(out, f)
		}
	}
	return out
}

// severityLabel is a colored badge when PRism can serve one, else bold text.
// The image keeps the word as alt text so text-only surfaces still read it.
func severityLabel(severity, badgeBase string) string {
	sev := strings.ToUpper(severity)
	switch strings.ToLower(severity) {
	case "critical", "medium", "low":
	default:
		badgeBase = ""
	}
	if badgeBase == "" {
		return "**[" + sev + "]**"
	}
	return fmt.Sprintf(`<img alt="%s" src="%s/%s.svg">`, sev, strings.TrimSuffix(badgeBase, "/"), strings.ToLower(severity))
}

// lowerSeverityNotes are confirmed findings below the inline bar: shown
// folded so the summary stays short but nothing confirmed is hidden.
func (r Round) lowerSeverityNotes() []payload.Finding {
	var notes []payload.Finding
	for _, f := range r.Findings {
		if Publishable(f) && !Shown(f) {
			notes = append(notes, f)
		}
	}
	sortBySeverity(notes)
	return notes
}

// unverifiedNotes are the active first-pass claims the agent left unverified
// or disputed, shown folded with a status marker when the policy allows.
func (r Round) unverifiedNotes() []payload.Finding {
	if !r.ShowUnverified {
		return nil
	}
	var notes []payload.Finding
	for _, f := range r.Findings {
		if UnverifiedNote(f) {
			notes = append(notes, f)
		}
	}
	sortBySeverity(notes)
	return notes
}

const maxFoldedNotes = 8

// writeFolded renders one details block of bullets. The count cap applies
// only when the rest has somewhere to go; the byte cap always holds, since
// GitHub rejects oversized bodies.
func (r Round) writeFolded(b *strings.Builder, label string, notes []payload.Finding, bullet func(payload.Finding) string) {
	if len(notes) == 0 {
		return
	}
	fmt.Fprintf(b, "<details><summary>%d %s%s</summary>\n\n", len(notes), label, plural(len(notes)))
	for i, f := range notes {
		overCount := r.DashboardURL != "" && i == maxFoldedNotes
		overBytes := b.Len() > SummaryMaxChars-600
		if overCount || overBytes {
			if r.DashboardURL != "" {
				fmt.Fprintf(b, "- ... %d more on the [dashboard](%s)\n", len(notes)-i, r.DashboardURL)
			} else {
				fmt.Fprintf(b, "- ... %d more omitted\n", len(notes)-i)
			}
			break
		}
		b.WriteString(bullet(f))
	}
	b.WriteString("</details>\n\n")
}

// RenderSummary is the sticky comment: a confidence line, the round diff, and
// one bullet per finding above the bar (critical first). Confirmed findings
// below the bar are listed folded under one line, unverified first-pass claims
// under another; rejected and merged records never appear.
func RenderSummary(r Round, sel Selection) string {
	shown := r.currentFindings()
	sortBySeverity(shown)
	critical, medium := 0, 0
	for _, f := range shown {
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

	var b strings.Builder
	b.WriteString(SummaryMarker + "\n")
	fmt.Fprintf(&b, "### PRism review: merge confidence %d/5\n", confidence)
	b.WriteString(recommendation(confidence) + "\n\n")
	if r.RoundNumber > 1 {
		d := r.diff()
		fmt.Fprintf(&b, "**Since last review:** %d new · %d still open · %d fixed\n\n", d.New, d.StillOpen, d.Fixed)
	}
	for _, f := range shown {
		if b.Len() > SummaryMaxChars-600 {
			fmt.Fprintf(&b, "- ... more on the [dashboard](%s)\n", r.DashboardURL)
			break
		}
		b.WriteString(r.bullet(f))
	}
	if len(shown) > 0 {
		b.WriteString("\n")
	}
	r.writeFolded(&b, "lower-severity note", r.lowerSeverityNotes(), r.bullet)
	r.writeFolded(&b, "unverified note", r.unverifiedNotes(), r.unverifiedBullet)
	if r.DashboardURL != "" {
		fmt.Fprintf(&b, "[Full report](%s)\n\n", r.DashboardURL)
	}
	fmt.Fprintf(&b, "<sub>Reviews (%d) · reviewed %s", r.RoundNumber, shortSHA(r.HeadSHA))
	if n := len(r.Commentable); n > 0 {
		fmt.Fprintf(&b, " · %d changed file%s", n, plural(n))
	}
	b.WriteString("</sub>\n")
	return b.String()
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

var kindLabels = map[string]string{
	"production_behavior": "Behavior change",
	"security_risk":       "Security",
	"latent_hazard":       "Latent hazard",
	"operational_risk":    "Operational risk",
	"test_quality":        "Test quality",
	"design_opinion":      "Design",
	"description_drift":   "Description drift",
}

var suggestionFenceRe = regexp.MustCompile("(?s)```suggestion\n.*?\n```")

// headline is the compact one-liner: kind and effect from the contract when
// the agent supplied one, else the comment's first sentence.
func headline(f payload.Finding) string {
	if c := f.FindingContract; c != nil && f.FindingContractStatus == "valid" && strings.TrimSpace(c.CurrentImpact) != "" {
		impact := strings.TrimSpace(c.Headline)
		if impact == "" {
			impact = clauseHeadline(strings.TrimSuffix(strings.TrimSpace(c.CurrentImpact), "."), headlineMaxRunes)
		}
		if label, ok := kindLabels[c.FindingKind]; ok {
			return label + " · " + impact
		}
		return impact
	}
	return truncateWords(strings.Trim(firstSentence(commentText(f)), "*_ "), 100)
}

const headlineMaxRunes = 110

// clauseBoundaries are the joints an effect sentence is most often built
// around; cutting at the last one before the limit keeps a headline readable.
var clauseBoundaries = []string{", so ", "; ", " because ", ", which ", ", and ", ", causing ", ", leaving ", ", "}

// clauseHeadline returns the whole sentence when it fits, else the longest
// prefix ending at a clause boundary within the limit, else a word-boundary cut.
func clauseHeadline(s string, max int) string {
	if len([]rune(s)) <= max {
		return s
	}
	window := string([]rune(s)[:max])
	best := -1
	for _, b := range clauseBoundaries {
		if i := strings.LastIndex(window, b); i > best && i >= max/3 {
			best = i
		}
	}
	if best > 0 {
		return strings.TrimRight(window[:best], " ,;")
	}
	return truncateWords(s, max)
}

// headlineIsCut reports whether the impact sentence still needs to be shown
// under the title: the title came from the agent's headline, or was cut.
func headlineIsCut(f payload.Finding) bool {
	c := f.FindingContract
	if c == nil || f.FindingContractStatus != "valid" {
		return false
	}
	if strings.TrimSpace(c.Headline) != "" {
		return true
	}
	impact := strings.TrimSuffix(strings.TrimSpace(c.CurrentImpact), ".")
	return clauseHeadline(impact, headlineMaxRunes) != impact
}

// RenderInline keeps the visible part Greptile-sized: headline, one
// calibration sentence, and the suggestion if there is one. The agent's full
// reasoning and the verification steps fold behind a details block.
func RenderInline(f payload.Finding, sourceTag string, agentLinkBase string, badgeBase string) string {
	comment := commentText(f)
	c := f.FindingContract
	hasContract := c != nil && f.FindingContractStatus == "valid"
	compact := hasContract && strings.TrimSpace(c.CurrentImpact) != ""

	var b strings.Builder
	b.WriteString(FindingMarker(f.ID) + "\n")
	if badgeBase == "" {
		fmt.Fprintf(&b, "**[%s] %s**\n", strings.ToUpper(f.Severity), headline(f))
	} else {
		fmt.Fprintf(&b, "%s **%s**\n", severityLabel(f.Severity, badgeBase), headline(f))
	}

	if compact && headlineIsCut(f) {
		b.WriteString("\n" + strings.TrimSpace(c.CurrentImpact) + "\n")
	}
	if hasContract && strings.TrimSpace(c.Uncertainty) != "" {
		b.WriteString("\n" + strings.TrimSpace(c.Uncertainty) + "\n")
	}
	if fence := suggestionFenceRe.FindString(comment); fence != "" {
		b.WriteString("\n" + fence + "\n")
	}

	reasoning := strings.TrimSpace(suggestionFenceRe.ReplaceAllString(comment, "*(suggestion above)*"))
	if !compact {
		// Without an impact sentence the headline came from the comment's first
		// sentence; the rest of the comment is the only explanation, so show it.
		sentence := firstSentence(comment)
		if rest := strings.TrimSpace(strings.TrimPrefix(reasoning, sentence)); rest != "" {
			b.WriteString("\n" + rest + "\n")
		}
		reasoning = ""
	}

	var details strings.Builder
	if reasoning != "" {
		details.WriteString(reasoning + "\n")
	}
	if hasContract && c.Falsifiability == "falsifiable" && c.FalsifiableCondition != nil && c.ExpectedObservable != nil {
		condition := strings.TrimSuffix(strings.TrimSpace(*c.FalsifiableCondition), ".")
		observable := strings.TrimSuffix(strings.TrimSpace(*c.ExpectedObservable), ".")
		fmt.Fprintf(&details, "\n**How to verify:** %s. Expected: %s.\n", condition, observable)
	}
	if agentLinkBase != "" {
		fmt.Fprintf(&details, "\nAgent prompt:\n```text\n%s\n```\n", agentPrompt(agentLinkBase, f))
	}
	if details.Len() > 0 {
		b.WriteString("\n<details><summary>Reasoning and how to verify</summary>\n\n" + details.String() + "</details>\n")
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

// summaryText is the table cell for a finding: the effect sentence from the
// contract when present, else the comment's first line.
func summaryText(f payload.Finding) string {
	if c := f.FindingContract; c != nil && f.FindingContractStatus == "valid" && strings.TrimSpace(c.CurrentImpact) != "" {
		return strings.TrimSpace(c.CurrentImpact)
	}
	return strings.Trim(firstLine(commentText(f)), "*_ ")
}

// truncateWords cuts at the last word boundary before max runes and appends
// an ellipsis; short strings are returned unchanged.
func truncateWords(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	cut := string(r[:max-3])
	if i := strings.LastIndex(cut, " "); i > 0 {
		cut = cut[:i]
	}
	return strings.TrimRight(cut, " ,;:") + "..."
}

// agentPrompt is the copyable plain-text equivalent of the agent link, for
// people not on Claude Code. The PR coordinates come from the link base.
func agentPrompt(base string, f payload.Finding) string {
	q, _ := url.ParseQuery(strings.TrimPrefix(base[strings.Index(base, "?")+1:], "?"))
	repo := q.Get("o") + "/" + q.Get("r") + "#" + q.Get("n")
	where := f.File
	if f.Line > 0 {
		where = fmt.Sprintf("%s:%d", f.File, f.Line)
	}
	effect := summaryText(f)
	return fmt.Sprintf("PRism finding on %s in %s: %s Read the review comment marked %s on that PR, decide whether it is valid, and fix it if so; otherwise explain why not.", where, repo, strings.TrimSpace(effect), FindingMarker(f.ID))
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
