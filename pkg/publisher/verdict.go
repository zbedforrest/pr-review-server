package publisher

import (
	"context"
	"regexp"
	"strings"

	"pr-review-server/db"
)

// OutcomeSettledVerdict is the terminal outcome of a reply the author verdict
// fast path handled: the finding was dismissed and the comment thumbed up, and
// the reply model never ran.
const OutcomeSettledVerdict = "settled:verdict"

// OutcomeShadowedVerdict marks a verdict seen under shadow: the comment was
// thumbed up but the finding was not dismissed, so a later respond scan
// finishes the settlement.
const OutcomeShadowedVerdict = "settled:verdict:shadow"

// Verdict kinds, for telemetry.
const (
	VerdictKindReply      = "reply"
	VerdictKindThumbsDown = "thumbs_down"
)

// SettledVerdict is one author verdict the scan acted on without a model
// reply. AuthorCommentID is zero for a thumbs-down on the root.
type SettledVerdict struct {
	RepoOwner       string
	RepoName        string
	PRNumber        int
	Fingerprint     string
	RootCommentID   int64
	AuthorCommentID int64
	Kind            string
}

var (
	verdictQuoteLineRe = regexp.MustCompile(`(?m)^[ \t]*>.*$`)
	verdictLinkRe      = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`)
	verdictMentionRe   = regexp.MustCompile(`(^|\s)@[\w-]+`)
	verdictMarkupRe    = regexp.MustCompile("[*_~`]+")
	verdictSpaceRe     = regexp.MustCompile(`\s+`)

	// Openers that carry no content of their own: "yes, this is intentional",
	// "for the 5th time this is intended behavior".
	verdictFillerRe = regexp.MustCompile(`^(?:(?:yes|yep|yeah|yup|no|nope|nah|ok|okay|correct|right|agreed|ack|hmm|hm|ah|oh|well|so|hi|hey|thanks|thank you|thx|sure|fwiw|again|also|and|but|for the \S+ time|as (?:i )?(?:said|mentioned|noted|explained)(?: before| above| earlier)?|like i said|same as before|same answer)[\s,.!:;-]+)*`)

	verdictSubject = `(?:this|that|it|they|these|those|this one|that one|the \S+(?: \S+){0,2}?)`
	verdictCopula  = `(?:\s+(?:is|was|are|were|would be)|'s|’s)`
	verdictAdverbs = `(?:(?:all|both|actually|indeed|just|also|totally|entirely|very much|still|completely|really|definitely|certainly|absolutely|purely|fully|very) )*`

	// Form A: the verdict is the reply ("Intentional.", "By design for this
	// PR.", "Won't fix: matches mobile.", "Not changing this here.").
	verdictBareRe = regexp.MustCompile(`^(intentional(?:ly)?|intended|deliberate(?:ly)?|by design|as designed|on purpose|working as intended|wai|won'?t fix|wont ?fix|will not fix|not a bug|not an issue|not a problem|false positive|accepted risk|known and accepted|acceptable|accepted|expected|not changing|not changed|not going to change|keeping (?:this |it |that )?as[ -]is|leaving (?:this |it |that )?as[ -]is|left as[ -]is)\b`)
	// What may follow a bare verdict word: the end, punctuation, or a
	// connective. "Intended fix is in abc" is a fix claim, not a verdict.
	verdictBareFollowRe = regexp.MustCompile(`^(?:$|[,.;:!?)\-]|\s+(?:and|but|as|for|since|because|so|here|there|given|per|in|on|though|although|behaviou?r|change|state|correction|choice|decision|phased|difference|ordering|order|gap|that|this|it|we|i|the|a|an|to|by|match(?:es|ing|ed)?|same|like|see|not|no|if|when|while|with|which|what|where|how|why|you|your|we|our|they|their|he|she|its|at|from|of|only|again|still|too|also|unless|until|rather|instead|just|per)\b)`)
	// "Accepted" and "expected" on their own acknowledge as often as they
	// rule ("Accepted the suggestion"), so they need a connective or a risk
	// noun to count.
	verdictWeakBare     = map[string]bool{"accepted": true, "acceptable": true, "expected": true}
	verdictWeakFollowRe = regexp.MustCompile(`^(?:[,;:]\s*(?:and|but|as|for|since|because|given|per|risk|behaviou?r|known)\b|\s+(?:risk|behaviou?r|and|but|as|for|since|because|given|per)\b)`)
	// "fine" rules only with a design context ("fine as is"); bare it is an
	// agreement the model should read.
	verdictFineContextRe = regexp.MustCompile(`^\s+(?:as[ -]is|by design|for (?:this|now)|here|behaviou?r|to me)\b`)
	verdictNegatedDoneRe = regexp.MustCompile(`\b(?:not|never|no|without)\b|n't\b`)

	// Form B: subject, copula, verdict ("This is intended behavior.", "That
	// is the goal.", "The ordering is deliberate.").
	verdictPredicateRe = regexp.MustCompile(`^` + verdictSubject + verdictCopula + `\s+` + verdictAdverbs + `(intentional(?:ly)?|intended|deliberate(?:ly)?|by design|as designed|on purpose|expected|acceptable|accepted|fine|desired|wanted|working as intended|wai|not a bug|not an issue|not a problem|a false positive|a non-issue|a feature|(?:correct|ok|okay|expected|desired|intended|wanted|normal|right) behaviou?r|what (?:we|product|the ticket|the spec|design|the designs?) (?:want|wants|wanted|asked(?: for)?|intend|intends|intended|specified|specifies)|the (?:goal|intent|intention|point|desired behaviou?r|expected behaviou?r|intended behaviou?r)|an? (?:intentional|intended|deliberate|conscious|design) (?:choice|decision|behaviou?r|change|trade-?off)|a known (?:gap|limitation|trade-?off)|an? accepted (?:risk|gap|limitation|trade-?off))\b`)

	// Form C: something was done on purpose ("This was done on purpose to
	// trigger a fresh fetch", "I removed the dedupe deliberately").
	verdictDoneRe = regexp.MustCompile(`^(?:` + verdictSubject + verdictCopula + `\s+(?:done|added|left|kept|made|set|written|wired|placed|removed|omitted|excluded|skipped|there|here|like that|that way|this way)\s+(on purpose|intentionally|deliberately|by design)|(?:i|we) (?:\S+ ){0,5}?(on purpose|intentionally|deliberately))\b`)

	// A hedge up to the verdict, or in its own clause, makes it an opinion.
	verdictHedgeRe = regexp.MustCompile(`\b(?:i think|i believe|i guess|i assume|i suppose|i'd say|i'd guess|not sure|unsure|maybe|perhaps|probably|possibly|might be|may be|could be|should be|iirc|afaik|i don'?t think|i doubt|i'?m not sure|if i recall|seems?|seemed|looks like|appears?)\b`)
	// A fix commitment in the same sentence means the author is changing the
	// code after all ("That's fine, I'll fix it anyway").
	verdictFixRe = regexp.MustCompile(`\b(?:i'?ll|i will|we'?ll|we will|will|let me|going to|gonna)\s+(?:fix|change|update|remove|add|revert|address|drop|rework|adjust|rename|move|apply|push|clean(?: it)? up)\b|(?:^|[,;:]\s*|\b(?:i|we|so|and|but)(?:'ve| have|'m| am|'re| are)? )(?:fixed|fixing|changed|changing|updated|updating|removed|removing|reverted|reverting|addressed|addressing|reworked|applied|applying|pushed|pushing)\b|\b(?:but|however|though|although|except)\b[^.]*\b(?:bug|wrong|broken|incorrect|a (?:real )?problem|an? (?:real )?issue|real gap)\b`)
	// A modal fix request ("but it should be fixed", "we need to change
	// this") is a commitment too; the subject guard keeps "a bigger hit area
	// should change both together" out of it.
	verdictModalFixRe = regexp.MustCompile(`(?:^|\b(?:it|this|that|these|those|we|i|which|they|you|the \S+(?: \S+){0,2}?))\s+(?:should|needs? to|must|ought to|has to|have to)\s+(?:be\s+)?(?:fixed|changed|updated|removed|reverted|addressed|fix|change|update|remove|revert|address)\b|\b(?:please|kindly)\s+(?:fix|change|update|remove|revert|address|rename|adjust|drop|rework)\b`)
	// A later sentence that calls something broken keeps the model path even
	// without a contrast word ("Expected. The new path is broken.").
	verdictDefectRe = regexp.MustCompile(`\b(?:is|are|was|were)\s+(?:(?:actually|still|indeed|also|genuinely|really)\s+)?(?:a\s+)?(?:real\s+)?(?:bug|broken|wrong|incorrect|problem|issue|regression)\b|\b(?:a |the )?real (?:bug|issue|problem|gap)\b`)
	// Editing the PR text is not a code fix ("That is the goal. I updated the
	// description.").
	verdictDocFixRe = regexp.MustCompile(`\b(?:fixed|fixing|changed|changing|updated|updating|added|adding|will update|i'?ll update)\s+(?:the |a |an )?(?:pr |ticket )?(?:description|title|docstring|comment|docs?|readme|changelog|note)\b`)
	// An opinion anywhere after the verdict ("this is intentional, i think");
	// modal verbs there describe behaviour ("should be fine") and stay out.
	verdictTrailingHedgeRe = regexp.MustCompile(`\b(?:i think|i believe|i guess|i assume|i suppose|i'd say|i'd guess|not sure|unsure|i'?m not sure|maybe|perhaps|probably|possibly|iirc|afaik|i don'?t think|i doubt|if i recall|seems?|seemed|looks like|appears?)\b`)
	// A first sentence that denies or fixes stops the scan from reading a
	// verdict out of the second one ("Not intentional. Intended fix is in
	// abc").
	verdictBlockRe = regexp.MustCompile(`n't\b|\b(?:not|never|no longer|unintentional|unintended|accident|accidental|accidentally|mistake|oversight|bug|typo|fixed|fixing|fix)\b`)

	verdictSecondSentenceMaxWords = 10
)

// IsVerdict reports whether an author's reply settles the finding in as many
// words: intentional, by design, won't fix, accepted risk, not a bug, or a
// close variant, as the point of the reply rather than a passing mention. It
// reads the first sentence, or the second when the first is short and
// neither denies nor fixes anything, and refuses hedges ("I think this is
// intentional"), deferrals ("intended to be done in a follow-up") and fix
// commitments in the same breath. Anything it refuses goes to the reply
// model as before.
func IsVerdict(body string) bool {
	sentences := verdictSentences(body)
	if len(sentences) == 0 {
		return false
	}
	at := -1
	switch {
	case verdictSentence(sentences[0]):
		at = 0
	case len(sentences) < 2 || verdictBlockRe.MatchString(sentences[0]) || len(strings.Fields(sentences[0])) > verdictSecondSentenceMaxWords:
		return false
	case verdictSentence(sentences[1]):
		at = 1
	default:
		return false
	}
	for _, later := range sentences[at+1:] {
		if claimsFix(later) || verdictDefectRe.MatchString(later) {
			return false
		}
	}
	return true
}

// claimsFix reports whether a sentence says the code was, will be or should
// be changed; a verdict followed by one ("Expected. Fixed in abc1234") keeps
// the fix claim path.
func claimsFix(sentence string) bool {
	sentence = verdictDocFixRe.ReplaceAllString(sentence, " ")
	return verdictFixRe.MatchString(sentence) || verdictModalFixRe.MatchString(sentence)
}

// verdictSentences returns the reply's sentences, lowercased, with quoted
// lines, code, links, mentions, template comments and markup removed.
func verdictSentences(body string) []string {
	text := htmlCommentRe.ReplaceAllString(body, " ")
	text = verdictQuoteLineRe.ReplaceAllString(text, " ")
	text = codeSpanRe.ReplaceAllString(text, " code ")
	text = verdictLinkRe.ReplaceAllString(text, "$1")
	text = verdictMentionRe.ReplaceAllString(text, " ")
	text = verdictMarkupRe.ReplaceAllString(text, "")
	var out []string
	for _, s := range sentenceEndRe.Split(strings.ToLower(text), -1) {
		s = strings.TrimSpace(verdictSpaceRe.ReplaceAllString(s, " "))
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

func verdictSentence(sentence string) bool {
	s := verdictFillerRe.ReplaceAllString(sentence, "")
	if s == "" {
		return false
	}
	keyword, rest, ok := matchVerdict(s)
	if !ok || claimsFix(rest) || verdictDefectRe.MatchString(rest) {
		return false
	}
	if keyword == "fine" && !verdictFineContextRe.MatchString(rest) {
		return false
	}
	clause, _, _ := strings.Cut(rest, ",")
	if verdictHedgeRe.MatchString(s[:len(s)-len(rest)]+clause) || verdictTrailingHedgeRe.MatchString(rest) {
		return false
	}
	if verdictNegatedDoneRe.MatchString(s[:len(s)-len(rest)-len(keyword)]) {
		return false
	}
	if (strings.HasSuffix(keyword, "intended") || strings.HasSuffix(keyword, "expected")) && strings.HasPrefix(rest, " to ") {
		return false
	}
	return true
}

// matchVerdict returns the verdict keyword a sentence opens with and what
// follows it.
func matchVerdict(s string) (keyword, rest string, ok bool) {
	if m := verdictBareRe.FindStringSubmatchIndex(s); m != nil {
		keyword, rest = s[m[2]:m[3]], s[m[1]:]
		if verdictWeakBare[keyword] {
			if verdictWeakFollowRe.MatchString(rest) {
				return keyword, rest, true
			}
		} else if verdictBareFollowRe.MatchString(rest) {
			return keyword, rest, true
		}
	}
	if m := verdictPredicateRe.FindStringSubmatchIndex(s); m != nil {
		return s[m[2]:m[3]], s[m[1]:], true
	}
	if m := verdictDoneRe.FindStringSubmatchIndex(s); m != nil {
		for i := 2; i+1 < len(m); i += 2 {
			if m[i] >= 0 {
				return s[m[i]:m[i+1]], s[m[1]:], true
			}
		}
	}
	return "", "", false
}

var htmlCommentRe = regexp.MustCompile(`(?s)<!--.*?-->`)

// settleVerdict acts on an author verdict without running the reply model:
// the finding is dismissed, the author's comment gets a thumbs-up, and the
// reply row is recorded with OutcomeSettledVerdict so no text step ever runs
// on it. React and respond both dismiss, the same modes under which a
// thumbs-down writes contested. Shadow reacts but leaves the finding's state
// alone, like the rest of its paths, and records OutcomeShadowedVerdict so a
// later respond scan dismisses the finding. A new row another leader
// recorded first is left to that leader, so one verdict is reported once. Every write is idempotent and the outcome goes
// last, so a failure part way is finished by the next scan, which sees a row
// with no outcome. The dismissal is keyed by the thread's fingerprint, which is what
// the publisher checks before posting any later wording of the finding.
func (r ReplyReactor) settleVerdict(ctx context.Context, t db.PublishedReplyTarget, state PRState, reply AuthorReply, row db.PublishedReply, handled bool, rep *ReplyReport) error {
	dismisses := r.Mode == ReplyModeReact || r.Mode == ReplyModeRespond
	if dismisses {
		if err := r.Ledger.SetPublishedFindingState(t.RepoOwner, t.RepoName, t.PRNumber, reply.Fingerprint, db.PublishedStateDismissed); err != nil {
			return err
		}
	}
	if !handled || row.Action != ReplyActionReacted {
		if err := r.GH.React(ctx, t.RepoOwner, t.RepoName, reply.CommentID); err != nil {
			return err
		}
		rep.Reacted++
	}
	switch {
	case !handled:
		row = db.PublishedReply{
			RepoOwner: t.RepoOwner, RepoName: t.RepoName, PRNumber: t.PRNumber,
			RootCommentID: reply.RootCommentID, AuthorCommentID: reply.CommentID,
			Fingerprint: reply.Fingerprint, AuthorID: state.AuthorID,
			Class: string(reply.Class), Action: ReplyActionReacted, Body: reply.Body, CreatedAt: reply.CreatedAt,
			DeferredTo: strings.Join(DeferredTickets(reply.Body), ","),
		}
		created, err := r.Ledger.RecordPublishedReply(&row)
		if err != nil {
			return err
		}
		if !created {
			return nil
		}
		rep.Recorded++
		rep.Handled = append(rep.Handled, row)
	case row.Action != ReplyActionReacted:
		if err := r.Ledger.SetPublishedReplyAction(t.RepoOwner, t.RepoName, t.PRNumber, reply.CommentID, ReplyActionReacted); err != nil {
			return err
		}
		row.Action = ReplyActionReacted
		rep.Handled = append(rep.Handled, row)
	}
	outcome := OutcomeShadowedVerdict
	if dismisses {
		outcome = OutcomeSettledVerdict
	}
	if err := r.Ledger.SetPublishedReplyOutcome(t.RepoOwner, t.RepoName, t.PRNumber, reply.CommentID, outcome); err != nil {
		return err
	}
	if dismisses {
		rep.Verdicts = append(rep.Verdicts, SettledVerdict{RepoOwner: t.RepoOwner, RepoName: t.RepoName, PRNumber: t.PRNumber,
			Fingerprint: reply.Fingerprint, RootCommentID: reply.RootCommentID, AuthorCommentID: reply.CommentID, Kind: VerdictKindReply})
	}
	return nil
}
