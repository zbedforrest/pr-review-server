package publisher

import (
	"os"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"pr-review-server/pkg/reviewer/payload"
)

// RenderHygieneEnv is the kill switch for the rendering hygiene of the
// 2026-10-13 audit: confidence labels and internal finding ids stripped
// from the contract sentences, praise never used as a title, the source
// footer dropped, folded notes counted on the status line and a refresh
// that edits nothing when the summary did not change. "false", "0", "off"
// and "no" restore the previous rendering.
const RenderHygieneEnv = "PUBLISH_RENDER_HYGIENE"

// RenderHygieneEnabled reads the kill switch at call time, like the discount
// gate, because the renderer has no policy in hand.
func RenderHygieneEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(RenderHygieneEnv))) {
	case "false", "0", "off", "no":
		return false
	}
	return true
}

// InlineOperationalRiskEnv lets an operational-risk finding with a current
// impact (a broken build, a failed deploy, a job that stops running) be shown
// and posted inline like a behavior finding. Off by default: it widens what
// posts, so it is meant to run with the quality gate on. "true", "1", "on"
// and "yes" turn it on.
const InlineOperationalRiskEnv = "PUBLISH_INLINE_OPERATIONAL_RISK"

// InlineOperationalRiskEnabled reads the switch at call time, because Shown
// has no policy in hand.
func InlineOperationalRiskEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(InlineOperationalRiskEnv))) {
	case "true", "1", "on", "yes":
		return true
	}
	return false
}

// GateEndorsedInlineEnv lets a finding the applied quality gate graded GOOD be
// shown and posted inline whatever its severity or contract, ranked below
// MEDIUM and CRITICAL and still under the inline caps. Off by default; it
// only acts with QUALITY_GATE=on, since shadow grades are never applied.
const GateEndorsedInlineEnv = "PUBLISH_GATE_GOOD_INLINE"

func gateEndorsed(f payload.Finding) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(GateEndorsedInlineEnv))) {
	case "true", "1", "on", "yes":
	default:
		return false
	}
	v := f.Verification
	return v != nil && v.GateApplied && v.Gate == "GOOD"
}

// NoteProblemClauseEnv is the kill switch for the problem-clause cut and
// the context-sentence skip: "false", "0", "off" and "no" restore a cut
// from the right and a title taken from the first sentence whatever it
// quotes.
const NoteProblemClauseEnv = "PUBLISH_NOTE_PROBLEM_CLAUSE"

// NoteProblemClauseEnabled reads its kill switch at call time, like
// RenderHygieneEnabled, because the renderer has no policy in hand.
func NoteProblemClauseEnabled() bool { return !envOff(NoteProblemClauseEnv) }

// ProblemClauseStartEnv is the kill switch for the standalone problem
// clause: a headline, bullet or inline title whose leading clause was
// dropped starts at the problem clause, capitalised, with no marker, and
// no rendered line opens with the marker or a lowercase connective.
// "false", "0", "off" and "no" restore the "[…] but ..." form.
const ProblemClauseStartEnv = "PUBLISH_PROBLEM_CLAUSE_START"

// ProblemClauseStartEnabled reads its kill switch at call time, like
// NoteProblemClauseEnabled.
func ProblemClauseStartEnabled() bool { return !envOff(ProblemClauseStartEnv) }

// Hold reasons, as the Report and the hygiene telemetry name them.
const (
	HoldLibraryClaim    = "unverified_library_claim"
	HoldRegressionClaim = "uncited_regression_claim"
	HoldPraise          = "praise"
	HoldReasonless      = "reasonless_note"
	// HoldVerifierHold and HoldVerifierDrop are the repo-evidence verifier's
	// applied decisions on a finding that has no open thread yet: neither
	// posts; the decision itself stays on the finding in the review sidecar.
	HoldVerifierHold = "verifier_hold"
	HoldVerifierDrop = "verifier_drop"
	HoldQualityGate  = "quality_gate"
)

// HeldFinding is a finding the output hygiene gate kept off GitHub this
// round; it stays on the dashboard and keeps alive any ledger row it
// aliases to.
type HeldFinding struct {
	ID     string
	File   string
	Line   int
	Reason string
}

// findingIDRefRe matches the labels the agent gives its own entries ("A-1",
// "FP-3", "P-2"); a sentence built on one means nothing to a reader. The
// label stands alone (not "schema-A-1") and has at most two digits, so a
// "P-256 curve" is left alone; a bare "FP-16" still reads as a label.
var findingIDRefRe = regexp.MustCompile(`(^|[^\w-])((?:A|FP|P)-\d{1,2})($|[^\w-])`)

// MentionsFindingID reports whether text leans on an internal finding label.
func MentionsFindingID(text string) bool {
	return findingIDRefRe.MatchString(text)
}

func replaceFindingIDs(text, with string) string {
	// The right boundary is consumed, so labels one space apart need a
	// second pass.
	for {
		next := findingIDRefRe.ReplaceAllString(text, "${1}"+with+"${3}")
		if next == text {
			return next
		}
		text = next
	}
}

// confidenceLabelRe is the "Low;" / "Medium:" / "High confidence," opener
// the agent puts before its uncertainty sentence. Under a severity badge it
// reads as a second, contradicting severity. A hyphen ends the label only
// with spaces around it, so "High-cardinality keys" is not a label.
var confidenceLabelRe = regexp.MustCompile(`(?i)^\s*(?:(?:very |fairly )?(?:high|medium|moderate|low|critical)(?:-(?:high|low))?(?: confidence)?|confidence(?: is)? (?:high|medium|moderate|low))(?:\s*[:;,]\s*|\s+-\s+)`)

// StripConfidenceLabel drops a leading confidence label and capitalises what
// is left. Text without a label is returned as is.
func StripConfidenceLabel(text string) string {
	loc := confidenceLabelRe.FindStringIndex(text)
	if loc == nil {
		return text
	}
	rest := strings.TrimSpace(text[loc[1]:])
	if rest == "" {
		return ""
	}
	r := []rune(rest)
	r[0] = unicode.ToUpper(r[0])
	return string(r)
}

// ContractSentence is a contract sentence as the renderer shows it: no
// confidence label, and nothing at all when the sentence is built on an
// internal finding id or is only a label.
func ContractSentence(text string) string {
	text = strings.TrimSpace(text)
	if !RenderHygieneEnabled() {
		return text
	}
	if MentionsFindingID(text) {
		return ""
	}
	return EscapeProseHTML(StripConfidenceLabel(text))
}

// bodyIDRefRe matches a parenthetical or trailing aside that only points at
// another entry: "(same as A-1)", "(see FP-2 and FP-3)", ", as in A-1".
var bodyIDRefRe = regexp.MustCompile(`(?i)\s*(?:\((?:see|same as|as in|like|cf\.?|per|similar to|same pattern as)?\s*(?:A|FP|P)-\d{1,2}(?:(?:\s*,\s*|\s+and\s+|\s*/\s*)(?:A|FP|P)-\d{1,2})*\)|,?\s+(?:as|like) in (?:A|FP|P)-\d{1,2}\b)`)

var findingIDListRe = regexp.MustCompile(`\b(?:A|FP|P)-\d{1,2}(?:(?:\s*,\s*|\s+and\s+|\s*/\s*|\s+)(?:A|FP|P)-\d{1,2})+\b`)

// StripFindingIDAsides removes the asides that point at other entries by
// their internal labels and names any label a sentence still leans on as
// another finding on the PR, which is all a reader can make of it.
func StripFindingIDAsides(text string) string {
	if !RenderHygieneEnabled() {
		return text
	}
	text = bodyIDRefRe.ReplaceAllString(text, "")
	text = findingIDListRe.ReplaceAllString(text, "other findings on this PR")
	return replaceFindingIDs(text, "another finding on this PR")
}

var (
	praiseOpenerRe   = regexp.MustCompile(`(?i)^(?:(?:the|this) (?:pr|change|patch|diff|code|implementation|approach|author|test|tests|migration|refactor|fix|new \w+)|it|you|the new \w+)\b[^.]{0,60}?\b(?:correctly|properly|appropriately|nicely|cleanly|rightly|safely|sensibly)\b|^(?:good|nice|great|solid|clean|well[- ]done|looks good|lgtm|no (?:issues?|problems?|concerns?|bugs?)(?: here| found| with| in)?)(?:[\s.,;:!]|$)|\b(?:is|are|looks?) (?:correct|fine|good|right|sound|solid|clean)\b|\bworks? as (?:intended|expected|designed)\b|\bis (?:a )?(?:correct|sound|sensible|reasonable|minimal|clean) (?:change|fix|approach|choice|implementation)\b`)
	praiseNegationRe = regexp.MustCompile(`(?i)\b(?:not|never|no longer|but|however|fails?|failing|missing|misses|incorrect(?:ly)?|wrong|except|unless|only|although|though|still|broken|breaks|should|needs?|lacks?|without|ignores?|ignoring|skips?|skipping|leaves?|leaving|bypass\w*|silently|regress\w*|revert\w*|leaks?|leaking|crash\w*|races?|racy|deadlock\w*|overflow\w*|corrupt\w*|panics?)\b|n't\b`)
	contrastOpenerRe = regexp.MustCompile(`(?i)^(?:however|but|that said|still|yet|nevertheless|nonetheless|even so|unfortunately|the (?:problem|issue|catch|gap|risk) is)\b[\s,:]*`)
)

// IsPraise reports whether a sentence compliments the code instead of
// reporting a defect: an approving verb on the PR or its code with no
// negation anywhere in the sentence.
func IsPraise(sentence string) bool {
	s := strings.TrimSpace(emphasisMarkers.Replace(sentence))
	if s == "" || praiseNegationRe.MatchString(s) {
		return false
	}
	return praiseOpenerRe.MatchString(s)
}

var sentenceSplitRe = regexp.MustCompile(`(?s)(.*?[.!?]['"”’]*(?:\*\*|__|\*|_)?['"”’]*)(?:\s+|$)`)

// sentences splits a comment (without its suggestion fence, lines folded)
// into sentences, keeping their end punctuation and any closing quote
// after it; the tail without punctuation is a sentence too.
func sentences(text string) []string {
	line := fold(suggestionFenceRe.ReplaceAllString(text, " "))
	var out []string
	rest := line
	for rest != "" {
		m := sentenceSplitRe.FindStringSubmatchIndex(rest)
		if m == nil {
			out = append(out, strings.TrimSpace(rest))
			break
		}
		if s := strings.TrimSpace(rest[m[2]:m[3]]); s != "" {
			out = append(out, s)
		}
		rest = rest[m[1]:]
	}
	return out
}

// FirstFindingSentence is the first sentence of a comment that reports
// something: a praise opener is skipped and the contrast word that follows
// it ("However, ...") is dropped. ok is false when the comment is praise all
// the way through.
func FirstFindingSentence(comment string) (string, bool) {
	ss := sentences(comment)
	i, ok := titleSentence(ss)
	if !ok {
		return "", false
	}
	if i == 0 {
		return ss[0], true
	}
	return capitalized(contrastOpenerRe.ReplaceAllString(ss[i], "")), true
}

// titleSentence picks the sentence that titles a comment: the first one,
// unless it is praise. After a compliment (often followed by context) it
// prefers a sentence that states a problem, then the first that is not
// praise. ok is false when the comment is praise all the way through.
func titleSentence(ss []string) (int, bool) {
	if len(ss) == 0 {
		return 0, false
	}
	if !RenderHygieneEnabled() || !IsPraise(ss[0]) {
		if len(ss) > 1 && NoteProblemClauseEnabled() && IsContext(ss[0]) && !StatesProblem(ss[0]) && standsAlone(ss[1]) {
			return 1, true
		}
		return 0, true
	}
	first := -1
	for i := 1; i < len(ss); i++ {
		s := contrastOpenerRe.ReplaceAllString(ss[i], "")
		if s == "" || IsPraise(s) {
			continue
		}
		if StatesProblem(s) {
			return i, true
		}
		if first < 0 {
			first = i
		}
	}
	return first, first >= 0
}

var anaphorOpenerRe = regexp.MustCompile(`(?i)^(?:this|that|these|those|it|its|they|their|such)\b`)

// standsAlone reports whether a sentence that follows a context sentence
// can title the comment: it opens with a contrast word, and what follows
// is neither praise nor an anaphor pointing back at the context.
func standsAlone(sentence string) bool {
	if !contrastOpenerRe.MatchString(sentence) {
		return false
	}
	rest := contrastOpenerRe.ReplaceAllString(sentence, "")
	return rest != "" && !IsPraise(rest) && !anaphorOpenerRe.MatchString(rest)
}

var contextOpenerRe = regexp.MustCompile(`(?i)^(?:(?:the|this|per the|according to the|as the)\s+)?(?:pr\s+|pull request\s+|commit\s+)?(?:description|body|summary|title|docstring|comment|changelog|message|ticket|issue|spec|readme)\b[^.]{0,40}?\b(?:states?|says?|said|claims?|notes?|reads?|describes?|mentions?|promises?|documents?|explains?|asserts?|calls? out|suggests?|implies)\b`)

// IsContext reports whether a sentence relays what the PR, its
// description or a comment says, as the setup for the problem the next
// sentence states: it opens by citing that text, or closes a quotation
// opened earlier in it. Whether it also states a problem is the caller's
// question (StatesProblem).
func IsContext(sentence string) bool {
	s := strings.TrimSpace(emphasisMarkers.Replace(sentence))
	if s == "" {
		return false
	}
	if contextOpenerRe.MatchString(s) {
		return true
	}
	last, n := utf8.DecodeLastRuneInString(s)
	return strings.ContainsRune(`'"”’`, last) && strings.ContainsAny(s[:len(s)-n], `'"“‘`)
}

func capitalized(s string) string {
	if s == "" {
		return s
	}
	r := []rune(s)
	r[0] = unicode.ToUpper(r[0])
	return string(r)
}

var (
	defectWordRe = regexp.MustCompile(`(?i)\b(?:fails?|failing|failed|missing|incorrect(?:ly)?|wrong|broken|break|breaks|ignore|ignores|ignoring|bypass(?:es|ed)?|silently|regress\w*|reverts?|reverting|leaks?|leaking|crash\w*|racy|deadlock\w*|overflows?|corrupt\w*|panics?|lose|loses|losing|lost|drifts?|drop|drops|dropped|overwrite|overwrites|overwritten|clobber\w*|truncat\w*)\b`)
)

// StatesProblem reports whether a sentence says what goes wrong, by a defect
// word, rather than only describing the code.
func StatesProblem(sentence string) bool {
	return defectWordRe.MatchString(sentence)
}

var (
	nilImpactOpenerRe = regexp.MustCompile(`(?i)^(?:none\b|nothing (?:today|changes|breaks|is broken|visible|observable)|no(?:t| current| direct| immediate| demonstrated| demonstrable| user[- ]visible| user[- ]facing| runtime| production| functional| observable| behaviou?r(?:al)?)? (?:impact|effect|change|difference|regression|consequence|user impact|runtime impact|behaviou?r change)|not? (?:demonstrated|user|runtime|production|current|direct|immediate|observable|functional|visible)\b)`)
	preferenceRe      = regexp.MustCompile(`(?i)\b(?:preference|not a defect|rather than a defect|no defect|not a bug|rather than a bug|not an? (?:issue|error|problem)|stylistic|style (?:call|choice|preference|nit)|matter of (?:taste|style)|judgment call|judgement call|convention(?:al)? (?:call|choice|preference)|purely cosmetic|cosmetic only|a nit\b)`)
)

// ReasonlessNote reports whether a finding would be listed as a bare title
// with nothing behind it: a design opinion titled by its headline (so the
// bullet carries no sentence of reasoning), whose impact sentence concedes
// no impact and whose severity rationale calls it a preference rather than
// a defect. A reader of the summary bullet would see a statement about the
// code and no reason to act.
func ReasonlessNote(f payload.Finding) bool {
	c := f.FindingContract
	if c == nil || f.FindingContractStatus != "valid" || c.FindingKind != "design_opinion" {
		return false
	}
	if strings.TrimSpace(c.Headline) == "" {
		return false
	}
	impact := StripConfidenceLabel(strings.TrimSpace(c.CurrentImpact))
	return nilImpactOpenerRe.MatchString(impact) && preferenceRe.MatchString(c.SeverityRationale)
}

var (
	memoryClaimRe = regexp.MustCompile(`(?i)\b(?:(?:recall(?:ed|ing)?|remember(?:ed|ing)?|quot(?:ed|ing)|cit(?:ed|ing)|writ(?:ten|ing)|reasoning|speaking|working|going|this is|that is|behaviou?r|docs?|documentation|defaults?|api|signature|semantics|contract) (?:this |that |it |the \w+ )?from memory\b|from memory,? (?:i|the (?:library|package|action|framework|middleware|plugin|sdk|module|default))\b|if i recall|iirc|as i recall|as i remember|as far as i remember|i believe the (?:library|package|action|module|framework|dependency|plugin|middleware|runtime)|without (?:inspecting|reading|checking|opening) the (?:pinned|installed|vendored|bundled|upstream|library|package|action|dependency)|(?:cannot|can't|could not|couldn't|did not|didn't|unable to) (?:inspect|read|check|open|verify|confirm) the (?:pinned|installed|vendored|bundled|upstream|action's|library's|package's|dependency's|module's)|(?:not|never) (?:inspected|checked|read|verified) (?:the )?(?:pinned|installed|vendored|upstream)|unverified against the pinned|unverified (?:default|behaviou?r|option|semantics)|documented behaviou?r from memory|(?:pinned|installed|vendored) (?:release|version|source|copy) (?:is|was) not (?:available|readable|inspected|checked)|not (?:available|present|readable) in this checkout|no (?:node_modules|vendor|site-packages) (?:in|available in) this checkout)`)
	// libraryEvidenceRe is the citation the AGENT_CLAIM_EVIDENCE prompt asks
	// for: a path and line under node_modules, vendor or site-packages, in a
	// lock file, or in a doc in the repo.
	libraryEvidenceRe = regexp.MustCompile(`(?i)(?:\b(?:node_modules|vendor|site-packages)/[\w./@-]+|[\w./@-]*(?:\.lock|-lock\.json|-lock\.yaml|go\.sum|go\.mod)|\b[\w./-]+\.(?:md|rst|txt|adoc)):\d+\b`)
	certainDefaultRe  = regexp.MustCompile(`(?i)(?:\b(?:[\w.@/-]+ )?(?:library|package|action|framework|middleware|plugin|sdk|dependency|module)(?:'s)? (?:defaults?|behaviou?r|semantics?|contract) (?:is|are) certain\b|\bdefaults? (?:is|are) certain\b|\bi am certain\b[^.]{0,40}\b(?:library|package|action|framework|middleware|plugin|sdk|module|defaults?|upstream)\b|\bcertain(?:ly)? (?:that )?the (?:library|package|action|framework|middleware|plugin|sdk|module)\b)`)
)

// UnverifiedLibraryClaim reports whether a finding rests on third-party
// behaviour the agent did not read: it says so (from memory, could not
// inspect the pinned release, "unverified against the pinned version" as
// the AGENT_CLAIM_EVIDENCE prompt instructs) or asserts a library's
// defaults as certain; "documented to" is a citation, not an admission. A claim like that was wrong in both audited cases;
// it is held, not posted. The rule is the agent's own admission: a plain
// uncited claim with no such wording is not held, since the publisher
// cannot tell a read default from a remembered one. A citation of the kind
// the prompt asks for (a pinned source, lock file or repo doc with a line)
// is the evidence the hold exists to demand, so it lifts the hold.
func UnverifiedLibraryClaim(f payload.Finding) bool {
	text := commentText(f)
	if c := f.FindingContract; c != nil {
		text += "\n" + c.Uncertainty + "\n" + c.CurrentImpact + "\n" + c.SeverityRationale
	}
	if libraryEvidenceRe.MatchString(text) {
		return false
	}
	return memoryClaimRe.MatchString(text) || certainDefaultRe.MatchString(text)
}

var (
	// dropClaimRe is a sentence that says the PR took something away and
	// quotes it: "dropped the branded precedence (`SITE.logo`)",
	// "no longer honours `current_logo`". The quoted span is the evidence
	// the claim rests on.
	dropClaimRe = regexp.MustCompile("(?i)\\b(?:(?:dropped|drops|removed|removes|lost|loses|discards|discarded) (?:the|its|this|that|all|every|each|any|an?)\\b|no longer (?:has|applies|honou?rs|checks|keeps|respects|guards|includes|uses|calls|passes|sets|reads|renders|returns|falls (?:back|through) to)|used to (?:have|apply|honou?r|check|keep|respect|guard|include|use|call|pass|set|read|render|return|fall (?:back|through) to)|previously (?:had|applied|honou?red|checked|kept|respected|guarded|included|used|called|passed|set|read|rendered|returned|fell (?:back|through) to))\\b[^`.]{0,80}?`([^`\\n]+)`")
	// conditionalRe marks a sentence about a hypothetical, not the PR.
	conditionalRe = regexp.MustCompile(`(?i)\b(?:a future|in future|if |would|could|were to|hypothetical|should (?:a|the|any)\b)`)
	// baseRefRe is the signal that a sentence compares the head to the base
	// branch rather than describing what the code does now.
	baseRefRe = regexp.MustCompile(`(?i)\b(?:previously|used to|no longer|on (?:master|main|the base branch)|before this (?:pr|change|patch|commit)|the (?:old|previous|prior|original)\b|was (?:previously|originally)|that (?:the|a|its|every|each) [^.]{0,40}?\b(?:keeps|kept|had|has|honou?rs|honou?red|applies|applied)\b|the (?:refactor|rewrite|port|pr|change|patch|diff|move|rename) (?:dropped|drops|removed|removes|lost|loses|discards|discarded))`)
)

// DropClaims lists the quoted spans a finding says the PR dropped, lost or
// no longer applies, read from its headline, impact sentence and the first
// three sentences of its comment. A sentence counts only when it compares
// the head to the base ("previously", "that the full logo keeps", "the
// refactor dropped"); "the extractor drops any call" describes the code now.
func DropClaims(f payload.Finding) []string {
	var parts []string
	if c := f.FindingContract; c != nil {
		parts = append(parts, c.Headline, c.CurrentImpact)
	}
	ss := sentences(commentText(f))
	if len(ss) > 3 {
		ss = ss[:3]
	}
	parts = append(parts, ss...)
	var spans []string
	for _, p := range parts {
		if !baseRefRe.MatchString(p) || conditionalRe.MatchString(p) {
			continue
		}
		for _, m := range dropClaimRe.FindAllStringSubmatch(p, -1) {
			if span := fold(m[1]); len(span) >= 3 {
				spans = append(spans, span)
			}
		}
	}
	return spans
}

// hunkLines splits a finding's hunk into the lines the diff removed and the
// lines it kept or added, whitespace-folded.
func hunkLines(hunk string) (removed, present []string) {
	for _, line := range strings.Split(hunk, "\n") {
		switch {
		case line == "" || strings.HasPrefix(line, "@@") || strings.HasPrefix(line, "---") || strings.HasPrefix(line, "+++"):
		case strings.HasPrefix(line, "-"):
			removed = append(removed, fold(line[1:]))
		case strings.HasPrefix(line, "+"):
			present = append(present, fold(line[1:]))
		default:
			present = append(present, fold(strings.TrimPrefix(line, " ")))
		}
	}
	return removed, present
}

func anyContains(lines []string, span string) bool {
	for _, l := range lines {
		if strings.Contains(l, span) {
			return true
		}
	}
	return false
}

// UncitedRegressionClaim reports a claim that the PR dropped some quoted
// code when the hunk the finding cites shows otherwise: the span sits on a
// context or added line and on no removed line, so what the claim says was
// taken away is still there. A span on a removed line is the citation the
// claim needs, and a span the hunk does not show at all is left alone,
// since the behaviour may have lived in another file.
func UncitedRegressionClaim(f payload.Finding) bool {
	spans := DropClaims(f)
	if len(spans) == 0 {
		return false
	}
	removed, present := hunkLines(f.DiffHunk)
	for _, span := range spans {
		if anyContains(removed, span) {
			return false
		}
	}
	for _, span := range spans {
		if anyContains(present, span) {
			return true
		}
	}
	return false
}

// commentAfterTitle is the comment without the sentences its title used up:
// the praise opener, if any, and the reporting sentence that titles it.
// When a sentence cannot be located again (it spanned a line break), the
// comment is returned whole rather than cut in the wrong place.
func commentAfterTitle(comment string) string {
	ss := sentences(comment)
	if len(ss) == 0 {
		return comment
	}
	t, ok := titleSentence(ss)
	if !ok {
		return ""
	}
	pos := 0
	var kept []string
	for i := 0; i <= t; i++ {
		j := strings.Index(comment[pos:], ss[i])
		if j < 0 {
			return comment
		}
		pos += j + len(ss[i])
		if i == t {
			break
		}
		if s := contrastOpenerRe.ReplaceAllString(ss[i], ""); s != "" && !IsPraise(ss[i]) && !IsPraise(s) {
			kept = append(kept, capitalized(s))
		}
	}
	return strings.TrimSpace(strings.Join(append(kept, strings.TrimSpace(comment[pos:])), " "))
}

// AllPraise reports whether a finding titled by its comment has nothing but
// praise to say.
func AllPraise(f payload.Finding) bool {
	if !titleFromComment(f) {
		return false
	}
	_, ok := FirstFindingSentence(commentText(f))
	return !ok && strings.TrimSpace(commentText(f)) != ""
}

// HoldReason names why the output hygiene gate keeps a finding off GitHub
// this round, or "" when it may post. Praise and reason-less notes are
// rendering hygiene; the two claim rules have their own switches.
func HoldReason(f payload.Finding, p Policy) string {
	// The verifier's decision covers folded notes too, so it is read before
	// the publishable check that only gates inline bullets.
	if reason := verifierHold(f); reason != "" {
		return reason
	}
	if !Publishable(f) {
		return ""
	}
	if RenderHygieneEnabled() {
		if AllPraise(f) {
			return HoldPraise
		}
		if ReasonlessNote(f) {
			return HoldReasonless
		}
	}
	if p.LibraryClaimHold && UnverifiedLibraryClaim(f) {
		return HoldLibraryClaim
	}
	if p.RegressionEvidence && UncitedRegressionClaim(f) {
		return HoldRegressionClaim
	}
	return ""
}

// verifierHold names the verifier's applied hold or drop on f. A CRITICAL
// is never dropped: the verifier's own validation turns such a drop into a
// hold, and the publisher reads it the same way should one arrive.
func verifierHold(f payload.Finding) string {
	v := f.Verification
	if !v.Suppresses() {
		return ""
	}
	if v.GateDrops() {
		return HoldQualityGate
	}
	if v.Refutes() && !strings.EqualFold(strings.TrimSpace(f.Severity), "critical") {
		return HoldVerifierDrop
	}
	return HoldVerifierHold
}

// NilImpactOpener reports whether an impact sentence concedes there is
// nothing to see today.
func NilImpactOpener(text string) bool {
	return nilImpactOpenerRe.MatchString(StripConfidenceLabel(strings.TrimSpace(text)))
}

func fold(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// HasConfidenceLabel reports whether a sentence opens with a confidence or
// severity label ("Low;", "Medium:"), the shape the renderer strips.
func HasConfidenceLabel(text string) bool {
	return confidenceLabelRe.MatchString(text)
}

// TitleOf is the one line a finding is listed by, for callers outside the
// package that compare rendered bodies against findings; it opens the way
// the rendered title does.
func TitleOf(f payload.Finding) string {
	if d := displayOf(f); d != nil {
		return EscapeProseHTML(strings.TrimSpace(d.Title))
	}
	return standaloneTitle(titleSource(f))
}
