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
	"pr-review-server/pkg/reviewer/reconcile"
	"pr-review-server/pkg/reviewer/types"
	"pr-review-server/pkg/textcut"
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

// ExternalMatch is a finding of this round that another reviewer's root
// thread already carries: the summary links to that root instead of PRism
// posting one of its own.
type ExternalMatch struct {
	Finding   payload.Finding
	Source    reconcile.Source
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
	// External is every other reviewer's root thread on the PR, parsed, as
	// listed when the round was built; the ledger policy re-lists it before
	// posting when GitHub allows. ExternalMatches is what the policy matched
	// this round, new or remembered, for the summary.
	External        []reconcile.ExternalFinding
	ExternalMatches []ExternalMatch
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
	// ProfileFooter names the review flavor in the summary footer ("PRism
	// Lite", "PRism Lite+ (custom)"); empty for an ordinary full review.
	ProfileFooter string
	// LegacyTitles is Policy.LegacyTitles as applied to this round;
	// LegacyClamp is Policy.LegacyClamp.
	LegacyTitles bool
	LegacyClamp  bool
	// SettleFooter is the rendered line under every root comment and the
	// summary (see the SettleFooter function); empty omits it.
	SettleFooter string

	// PriorComments is what GitHub currently shows for PRism's own inline
	// comments, keyed by the fingerprint in their marker: the translated line,
	// the body and PRism's own replies in the thread. The ledger policy
	// aliases reworded findings against it.
	PriorComments map[string]PriorComment
	// PRBody is the pull request description; a folded note it already
	// states is held (Policy.BodyCheck).
	PRBody string
	// Changes resolves what changed between a ledger row's LastSeenSHA and
	// HeadSHA; ok false means unknown, and nothing is then judged fixed.
	Changes func(base string) (ChangeSet, bool)
	// Scope is the review's diff scope (payload.Payload.Scope); nil when the
	// review did not record one. DiffFiles is the PR diff at review time as
	// GitHub lists it, nil when the caller did not have it. A finding on a
	// path the scope marks inherited from the base branch, or outside the PR
	// diff, makes no fixed, reopen or severity transition.
	Scope     *payload.ReviewScope
	DiffFiles map[string]bool

	// transitions is the ledger policy's record count for the summary; nil
	// falls back to the hash-set diff.
	transitions *roundDiff
	// held are the findings the output hygiene gate removed from Findings
	// this round; the counts treat them as present, never as shown.
	held map[string]bool
}

// PriorComment is one of PRism's own inline comments as GitHub lists it now,
// with the replies PRism itself left in its thread (concessions name the
// code they accept, which the settled-point alias reads).
type PriorComment struct {
	Line       int
	Text       string
	OwnReplies []string
}

// ChangeSet is the inter-push diff between two commits of the PR: the files
// touched and, when the caller had the patches, the RIGHT-side lines added
// or edited per file and the hunks of each file. Lines or Hunks nil means
// only the file set is known; Hunks set but empty for a file means the
// caller had patches but not that file's (too large, binary).
type ChangeSet struct {
	Files map[string]bool
	Lines map[string]map[int]bool
	Hunks map[string][]Hunk
}

// Hunk is one unified-diff hunk header: the LEFT-side (base) and RIGHT-side
// (head) line ranges it covers, each as a start and a count.
type Hunk struct {
	OldStart, OldLines int
	NewStart, NewLines int
}

// SettleFooter is the one line under every root comment and the summary:
// how an author settles a thread (only while author replies are handled)
// and where to stop the comments entirely. Empty when neither applies.
func SettleFooter(optOutURL string, repliesEnabled bool) string {
	var parts []string
	if repliesEnabled {
		parts = append(parts, "Reply <code>intentional</code> or <code>won't fix</code> to settle a thread")
	}
	if optOutURL != "" {
		parts = append(parts, fmt.Sprintf(`<a href="%s">Stop PRism comments on your PRs</a>`, optOutURL))
	}
	if len(parts) == 0 {
		return ""
	}
	return "<sub>" + strings.Join(parts, " · ") + "</sub>"
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
	if r.transitions != nil {
		return *r.transitions
	}
	present := map[string]bool{}
	for _, f := range r.activeClaims() {
		present[f.ID] = true
	}
	for id := range r.held {
		present[id] = true
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

// bulletWhereSep joins a bullet's text to its file link.
const bulletWhereSep = " \u2014 "

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
	text := r.clampBullet(summaryText(f, r.LegacyTitles))
	return fmt.Sprintf("- %s %s%s%s[`%s`](%s)\n", severityLabel(f.Severity, r.BadgeBaseURL), marker, text, bulletWhereSep, where, r.findingLink(f))
}

// bulletMaxRunes bounds one summary bullet's text.
const bulletMaxRunes = 200

// clampBullet is the bullet text: the trailing period dropped unless the
// agent ended on an ellipsis, then cut to the bullet limit.
func (r Round) clampBullet(text string) string {
	text = strings.TrimSpace(text)
	if !strings.HasSuffix(text, "...") && !strings.HasSuffix(text, cutMarker) {
		text = strings.TrimSuffix(text, ".")
	}
	return clampText(text, bulletMaxRunes, r.LegacyClamp)
}

const sinceLastReviewFormat = "**Since last review:** %d new · %d still open · %d fixed"

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
		line += "  - Agent: " + clampText(EscapeProseHTML(StripFindingIDAsides(firstLine(reason))), maxReasonRunes, r.LegacyClamp) + "\n"
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

// lowerSeverityNotes are confirmed findings below the inline bar, including
// a CRITICAL or MEDIUM the discount gate held back: shown folded so the
// summary stays short but nothing confirmed is hidden.
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
	confidence := r.confidence()

	var b strings.Builder
	b.WriteString(SummaryMarker + "\n")
	fmt.Fprintf(&b, "### PRism review: merge confidence %d/5\n", confidence)
	b.WriteString(recommendation(confidence) + "\n\n")
	if r.RoundNumber > 1 {
		d := r.diff()
		if r.transitions != nil {
			d = *r.transitions
		}
		fmt.Fprintf(&b, sinceLastReviewFormat, d.New, d.StillOpen, d.Fixed)
		// Folded notes have no ledger rows, so the three counts never see
		// them; naming them keeps "0 new" from sitting over a list of six.
		if n := len(r.lowerSeverityNotes()) + len(r.unverifiedNotes()); n > 0 && RenderHygieneEnabled() {
			fmt.Fprintf(&b, " · %d folded note%s", n, plural(n))
		}
		b.WriteString("\n\n")
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
	r.writeExternal(&b)
	r.writeFolded(&b, "folded note", r.lowerSeverityNotes(), r.bullet)
	r.writeFolded(&b, "unverified note", r.unverifiedNotes(), r.unverifiedBullet)
	if r.DashboardURL != "" {
		fmt.Fprintf(&b, "[Full report](%s)\n\n", r.DashboardURL)
	}
	fmt.Fprintf(&b, "<sub>Reviews (%d) · reviewed %s", r.RoundNumber, shortSHA(r.HeadSHA))
	if n := len(r.Commentable); n > 0 {
		fmt.Fprintf(&b, " · %d changed file%s", n, plural(n))
	}
	if r.ProfileFooter != "" {
		fmt.Fprintf(&b, " · Reviewed by %s", r.ProfileFooter)
	}
	b.WriteString("</sub>\n")
	if r.SettleFooter != "" {
		b.WriteString(r.SettleFooter + "\n")
	}
	return b.String()
}

// writeExternal lists the findings another reviewer's thread already
// carries, each linked to that thread, so the point is visible in the
// summary without a second root.
func (r Round) writeExternal(b *strings.Builder) {
	if len(r.ExternalMatches) == 0 {
		return
	}
	matches := make([]ExternalMatch, 0, len(r.ExternalMatches))
	seen := make(map[int64]bool, len(r.ExternalMatches))
	for _, m := range r.ExternalMatches {
		if m.CommentID != 0 && seen[m.CommentID] {
			continue
		}
		seen[m.CommentID] = true
		matches = append(matches, m)
	}
	sort.SliceStable(matches, func(i, j int) bool {
		a, c := matches[i].Finding, matches[j].Finding
		if ra, rc := severityRank(a.Severity), severityRank(c.Severity); ra != rc {
			return ra > rc
		}
		if a.File != c.File {
			return a.File < c.File
		}
		return a.Line < c.Line
	})
	fmt.Fprintf(b, "<details><summary>%d finding%s already raised in another reviewer's thread</summary>\n\n", len(matches), plural(len(matches)))
	for _, m := range matches {
		if b.Len() > SummaryMaxChars-600 {
			b.WriteString("- ... more omitted\n")
			break
		}
		f := m.Finding
		where := f.File[strings.LastIndex(f.File, "/")+1:]
		if f.Line > 0 {
			where = fmt.Sprintf("%s:%d", where, f.Line)
		}
		text := r.clampBullet(summaryText(f, r.LegacyTitles))
		link := fmt.Sprintf("https://github.com/%s/%s/pull/%d#discussion_r%d", r.Owner, r.Repo, r.Number, m.CommentID)
		fmt.Fprintf(b, "- %s %s, [`%s`](%s) · %s\n", severityLabel(f.Severity, r.BadgeBaseURL), text, where, link, m.Source.Label())
	}
	b.WriteString("</details>\n\n")
}

// FirstSentence returns the leading sentence of the comment's first line (a
// period inside an open quote or parenthesis does not end it), or the whole
// first line when it has no sentence boundary.
func FirstSentence(comment string) string {
	return firstSentence(comment)
}

func firstSentence(comment string) string {
	return textcut.FirstSentence(firstLine(comment))
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

var suggestionFenceRe = regexp.MustCompile("(?s)```suggestion\n(.*?)\n```")

// headline is the bold line of an inline comment: the kind label when the
// contract carries an effect sentence, then the finding's title cut to fit.
// The legacy form fell back to the bare kind label when the effect sentence
// did not fit and left findings without a headline titled by their impact
// sentence even when that sentence said there was none.
func headline(f payload.Finding, legacy bool) string {
	return headlineWith(f, legacy, false)
}

// headlineWith is headline with the cut rule chosen: the clause cut with
// the explicit marker, or the legacy word cut with "...".
func headlineWith(f payload.Finding, legacy, legacyClamp bool) string {
	if legacy {
		return legacyHeadlineWith(f, legacyClamp)
	}
	title := titleSource(f)
	if c := f.FindingContract; c != nil && f.FindingContractStatus == "valid" && strings.TrimSpace(c.CurrentImpact) != "" {
		cut := clauseHeadlineWith(title, contractTitleLimit(f, legacyClamp), legacyClamp)
		if label, ok := kindLabels[c.FindingKind]; ok {
			return label + " · " + cut
		}
		return cut
	}
	return clauseHeadlineWith(title, titleLimit(commentTitleMaxRunes, legacyCommentTitleMaxRunes, legacyClamp), legacyClamp)
}

// contractTitleLimit is the headline limit for a finding with a contract
// impact: the bullet limit when the title is that impact sentence, else the
// headline limit (the legacy clamp keeps its own).
func contractTitleLimit(f payload.Finding, legacyClamp bool) int {
	if legacyClamp {
		return legacyHeadlineMaxRunes
	}
	if titleFromImpact(f) {
		return impactTitleMaxRunes
	}
	return headlineMaxRunes
}

// commentTitleMaxRunes bounds a title taken from the comment's first
// sentence; headlineMaxRunes a contract headline, which the contract itself
// keeps within this length, so a headline the agent wrote renders whole.
// The legacy limits are what the renderer applied before the clamp with a
// marker (PUBLISH_RENDER_CLAMP_V2=false).
const (
	commentTitleMaxRunes       = 140
	legacyCommentTitleMaxRunes = 100
	legacyHeadlineMaxRunes     = 110
)

func titleLimit(current, legacy int, legacyClamp bool) int {
	if legacyClamp {
		return legacy
	}
	return current
}

// titleIsWholeFirstSentence reports whether a comment-sourced title fits the
// limit headline applied to it; a cut title leaves the sentence to the fold.
func titleIsWholeFirstSentence(f payload.Finding, limit int) bool {
	return len([]rune(titleSource(f))) <= limit
}

// legacyHeadlineWith is the pre-2026-10 headline with the cut rule chosen the
// same way as headlineWith, so both kill switches together restore the old
// word cut.
func legacyHeadlineWith(f payload.Finding, legacyClamp bool) string {
	if c := f.FindingContract; c != nil && f.FindingContractStatus == "valid" && strings.TrimSpace(c.CurrentImpact) != "" {
		label, hasLabel := kindLabels[c.FindingKind]
		impact := EscapeProseHTML(strings.TrimSpace(c.Headline))
		if impact == "" {
			full := EscapeProseHTML(strings.TrimSuffix(strings.TrimSpace(c.CurrentImpact), "."))
			impact = clauseHeadlineWith(full, legacyHeadlineMaxRunes, legacyClamp)
			if impact != full && hasLabel {
				return label
			}
		}
		if hasLabel {
			return label + " · " + impact
		}
		return impact
	}
	first := EscapeProseHTML(strings.Trim(firstSentence(commentText(f)), "*_ "))
	if legacyClamp {
		return truncateWords(first, 100)
	}
	return clampText(first, 100, false)
}

// titleSource is the one line a finding is known by: the agent's headline,
// else the first sentence of its effect statement when the contract asserts
// an impact that exists today, else the comment's first sentence. A finding
// whose materiality is unknown or nil must not be titled by a sentence
// saying so.
func titleSource(f payload.Finding) string {
	if c := f.FindingContract; c != nil && f.FindingContractStatus == "valid" {
		if h := ContractSentence(c.Headline); h != "" {
			return h
		}
		if titleFromImpact(f) {
			if impact := impactTitle(c); impact != "" {
				return impact
			}
		}
	}
	// A comment that opens with a compliment is titled by the sentence that
	// reports something; praise all the way through is held before this.
	if sentence, ok := FirstFindingSentence(commentText(f)); ok {
		if RenderHygieneEnabled() {
			sentence = StripConfidenceLabel(sentence)
		}
		return EscapeProseHTML(strings.TrimSpace(emphasisMarkers.Replace(StripFindingIDAsides(sentence))))
	}
	return EscapeProseHTML(strings.TrimSpace(emphasisMarkers.Replace(StripFindingIDAsides(firstSentence(commentText(f))))))
}

// impactTitle is the effect statement's first sentence as a title: cleaned
// like every contract sentence, without its final period, and empty when
// the sentence leans on an internal finding id.
func impactTitle(c *types.FindingContract) string {
	return strings.TrimSuffix(ContractSentence(firstSentence(c.CurrentImpact)), ".")
}

// titleFromImpact reports whether the title is the contract's effect
// statement: no headline, and an impact the contract asserts exists today.
func titleFromImpact(f payload.Finding) bool {
	c := f.FindingContract
	return c != nil && f.FindingContractStatus == "valid" && ContractSentence(c.Headline) == "" &&
		c.Materiality == "current_impact" && impactTitle(c) != ""
}

// impactTitleMaxRunes bounds a title that is the effect sentence itself: the
// bullet limit, so the inline headline and the summary bullet both carry the
// whole problem statement rather than a cut of it with the sentence repeated
// below.
const impactTitleMaxRunes = bulletMaxRunes

var emphasisMarkers = strings.NewReplacer("**", "", "__", "")

// titleFromComment reports whether the inline title was taken from the
// comment's first sentence rather than the contract.
func titleFromComment(f payload.Finding) bool {
	c := f.FindingContract
	if c == nil || f.FindingContractStatus != "valid" {
		return true
	}
	return ContractSentence(c.Headline) == "" && !titleFromImpact(f)
}

const headlineMaxRunes = 140

var clauseBoundaries = textcut.ClauseBoundaries

// clauseHeadline returns the whole sentence when it fits, else the longest
// prefix ending at a clause boundary within the limit, else a word-boundary cut.
func clauseHeadline(s string, max int) string {
	return clauseHeadlineWith(s, max, false)
}

func clauseHeadlineWith(s string, max int, legacyClamp bool) string {
	if len([]rune(s)) <= max {
		return s
	}
	if !legacyClamp {
		return clampText(s, max, false)
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

// cutMarker is what a cut bullet or headline ends with.
const cutMarker = textcut.Marker

// clampText cuts s to max runes the way textcut.Clamp does (a dropped
// parenthetical, else a sentence, clause or word boundary outside every
// open pair, ending with cutMarker), keeping the problem clause over the
// context clause under PUBLISH_NOTE_PROBLEM_CLAUSE. Legacy restores the
// word cut with "...". A text within the limit is returned unchanged.
func clampText(s string, max int, legacy bool) string {
	if legacy {
		return truncateWords(s, max)
	}
	return textcut.ClampWith(s, max, textcut.Options{KeepProblemClause: NoteProblemClauseEnabled()})
}

// impactBelowTitle is the part of the effect statement still owed under the
// title: nothing when the title is the statement in full, the sentences
// after the first when the title is that first sentence, otherwise the whole
// statement.
func impactBelowTitle(f payload.Finding, legacy, legacyClamp bool) string {
	c := f.FindingContract
	if c == nil || f.FindingContractStatus != "valid" {
		return ""
	}
	cleaned := ContractSentence(c.CurrentImpact)
	impact := strings.TrimSuffix(cleaned, ".")
	if legacy && strings.TrimSpace(c.Headline) != "" {
		return cleaned
	}
	if legacy {
		if clauseHeadline(impact, legacyHeadlineMaxRunes) != impact {
			return cleaned
		}
		return ""
	}
	title := titleSource(f)
	switch clauseHeadlineWith(title, contractTitleLimit(f, legacyClamp), legacyClamp) {
	case impact:
		return ""
	case impactTitle(c):
		return strings.TrimSpace(strings.TrimPrefix(cleaned, firstSentence(cleaned)))
	}
	return cleaned
}

// reasoningAfterTitle is the fold text of a comment-titled finding: the
// comment after the sentences its title used, with the suggestion fence
// replaced and internal labels stripped.
func reasoningAfterTitle(comment string) string {
	return StripFindingIDAsides(strings.TrimSpace(suggestionFenceRe.ReplaceAllString(commentAfterTitle(comment), "*(suggestion above)*")))
}

// RenderInline keeps the visible part Greptile-sized: headline, one
// calibration sentence, and the suggestion if there is one. The agent's full
// reasoning and the verification steps fold behind a details block.
func RenderInline(f payload.Finding, sourceTag string, agentLinkBase string, badgeBase string, settleFooter string) string {
	return renderInline(f, sourceTag, agentLinkBase, badgeBase, settleFooter, false, false)
}

func renderInline(f payload.Finding, sourceTag string, agentLinkBase string, badgeBase string, settleFooter string, legacy, legacyClamp bool) string {
	comment := EscapeProseHTML(commentText(f))
	c := f.FindingContract
	hasContract := c != nil && f.FindingContractStatus == "valid"
	compact := hasContract && strings.TrimSpace(c.CurrentImpact) != ""

	// What the title does not already say of the effect statement is shown
	// once under it.
	title := headlineWith(f, legacy, legacyClamp)
	var b strings.Builder
	b.WriteString(FindingMarker(f.ID) + "\n")
	if badgeBase == "" {
		fmt.Fprintf(&b, "**[%s] %s**\n", strings.ToUpper(f.Severity), title)
	} else {
		fmt.Fprintf(&b, "%s **%s**\n", severityLabel(f.Severity, badgeBase), title)
	}

	if compact {
		if impact := impactBelowTitle(f, legacy, legacyClamp); impact != "" {
			b.WriteString("\n" + impact + "\n")
		}
	}
	if hasContract {
		if uncertainty := ContractSentence(c.Uncertainty); uncertainty != "" {
			b.WriteString("\n" + uncertainty + "\n")
		}
	}
	if fence := suggestionFenceRe.FindString(comment); fence != "" {
		b.WriteString("\n" + fence + "\n")
	}

	reasoning := StripFindingIDAsides(strings.TrimSpace(suggestionFenceRe.ReplaceAllString(comment, "*(suggestion above)*")))
	if !compact {
		// Without an impact sentence the headline came from the comment's first
		// sentence; the rest of the comment is the only explanation, so show it.
		rest := reasoning
		if legacy {
			rest = strings.TrimSpace(strings.TrimPrefix(reasoning, firstSentence(comment)))
		} else if titleFromComment(f) && titleIsWholeFirstSentence(f, titleLimit(commentTitleMaxRunes, legacyCommentTitleMaxRunes, legacyClamp)) {
			rest = reasoningAfterTitle(comment)
		}
		if rest != "" {
			b.WriteString("\n" + rest + "\n")
		}
		reasoning = ""
	} else if !legacy && titleFromComment(f) && titleIsWholeFirstSentence(f, titleLimit(headlineMaxRunes, legacyHeadlineMaxRunes, legacyClamp)) {
		reasoning = reasoningAfterTitle(comment)
	}

	var details strings.Builder
	if reasoning != "" {
		details.WriteString(reasoning + "\n")
	}
	if hasContract && c.Falsifiability == "falsifiable" && c.FalsifiableCondition != nil && c.ExpectedObservable != nil {
		condition := EscapeProseHTML(strings.TrimSuffix(strings.TrimSpace(*c.FalsifiableCondition), "."))
		observable := EscapeProseHTML(strings.TrimSuffix(strings.TrimSpace(*c.ExpectedObservable), "."))
		fmt.Fprintf(&details, "\n**How to verify:** %s. Expected: %s.\n", condition, observable)
	}
	if agentLinkBase != "" {
		fmt.Fprintf(&details, "\nAgent prompt:\n```text\n%s\n```\n", agentPrompt(agentLinkBase, f, legacy))
	}
	if details.Len() > 0 {
		b.WriteString("\n<details><summary>Reasoning and how to verify</summary>\n\n" + details.String() + "</details>\n")
	}

	var subs []string
	// The source line named which reviewers raised the point; the summary
	// now links the other reviewer's thread instead, so the line only puzzled
	// readers.
	if !RenderHygieneEnabled() {
		switch sourceTag {
		case SourceTagBoth:
			subs = append(subs, "<sub>Source: PRism · Both</sub>")
		case SourceTagGreptileOnly:
			subs = append(subs, "<sub>Source: Greptile</sub>")
		}
	}
	if agentLinkBase != "" {
		subs = append(subs, fmt.Sprintf(`<sub><a href="%s">Fix with agent</a></sub>`, agentLink(agentLinkBase, f)))
	}
	if settleFooter != "" {
		subs = append(subs, settleFooter)
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

// summaryText is the bullet text for a finding: its title (see titleSource).
// The legacy form was the effect sentence whenever the contract had one, which
// produced bullets reading "None today; ..." for immaterial findings.
func summaryText(f payload.Finding, legacy bool) string {
	if !legacy {
		return titleSource(f)
	}
	if c := f.FindingContract; c != nil && f.FindingContractStatus == "valid" && strings.TrimSpace(c.CurrentImpact) != "" {
		return EscapeProseHTML(strings.TrimSpace(c.CurrentImpact))
	}
	return EscapeProseHTML(strings.Trim(firstLine(commentText(f)), "*_ "))
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
func agentPrompt(base string, f payload.Finding, legacy bool) string {
	q, _ := url.ParseQuery(strings.TrimPrefix(base[strings.Index(base, "?")+1:], "?"))
	repo := q.Get("o") + "/" + q.Get("r") + "#" + q.Get("n")
	where := f.File
	if f.Line > 0 {
		where = fmt.Sprintf("%s:%d", f.File, f.Line)
	}
	effect := strings.TrimSuffix(strings.TrimSpace(summaryText(f, legacy)), ".") + "."
	return fmt.Sprintf("PRism finding on %s in %s: %s Read the review comment marked %s on that PR, decide whether it is valid, and fix it if so; otherwise explain why not.", where, repo, effect, FindingMarker(f.ID))
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
