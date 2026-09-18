// Package replytext shapes the text PRism posts under an author's reply: it
// reads the author's words for the shape of their pushback and enforces the
// reply conventions the model is asked to follow but sometimes does not.
package replytext

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Context is what the renderer knows about the thread besides the reply body.
type Context struct {
	AuthorComment string
	FindingBody   string
	Decision      string
}

const (
	NoteIntentAcknowledged = "intent_acknowledged"

	TicketAsk   = "If there is a ticket for this, reply with its key and this thread can be closed against it."
	riskAskBare = "Should this be noted in the PR description as accepted risk?"
)

// agreementOpenerRe matches an agreement formula at the head of the body when
// it stands alone: followed by punctuation and a space, by a dash, by "that",
// or by nothing. "Right now the guard..." is not a formula and is left alone,
// nor is "right.go:41 rejects nil" (the period is a filename's), and neither
// is "You're right about the guard": dropping the formula there leaves a
// fragment no rule can turn back into a sentence, so the prompt alone covers
// that form.
var agreementOpenerRe = regexp.MustCompile(`(?i)^(?:i (?:think |believe |guess )?(?:you(?:'|’)?re right|you are right|agree)|you(?:'|’)?re right|you are right|that(?:'|’)?s (?:right|correct|fair)|correct|agreed|agree|good point|fair point|good catch|nice catch|fair enough|fair|right|yes|yep|indeed|exactly|true)(?:\s*[,.:;!]+(?:\s+|$)|\s+[—–-]+\s+|\s+that\s+|\s*$)`)

// A conjunction left behind by the formula ("Correct, and the fix is in")
// goes too, unless it heads a phrase of its own: "so long as", "so far",
// "and yet", "so that".
var (
	leadConjunctionRe = regexp.MustCompile(`(?i)^(?:and|but|so)\s+(\w+)`)
	conjunctionPhrase = map[string]bool{"long": true, "far": true, "yet": true, "then": true, "that": true, "if": true, "as": true, "forth": true, "much": true, "too": true, "on": true}
)

// StripAgreementOpener drops agreement formulas from the head of the body and
// capitalizes what remains. ok is false when nothing but formulas was there.
// A body with no formula is returned as written.
func StripAgreementOpener(body string) (out string, ok bool) {
	out = strings.TrimSpace(body)
	stripped := false
	for i := 0; i < 4; i++ {
		m := agreementOpenerRe.FindStringIndex(out)
		if m == nil {
			break
		}
		out, stripped = strings.TrimSpace(out[m[1]:]), true
		if c := leadConjunctionRe.FindStringSubmatch(out); c != nil && !conjunctionPhrase[strings.ToLower(c[1])] {
			out = strings.TrimSpace(out[len(c[0])-len(c[1]):])
		}
	}
	if out == "" {
		return "", false
	}
	if !stripped {
		return out, true
	}
	return capitalize(out), true
}

// capitalize upper-cases a leading lower-case word but leaves code alone: a
// path, an identifier or a camelCase name must stay searchable as written.
func capitalize(s string) string {
	r, size := utf8.DecodeRuneInString(s)
	if r == utf8.RuneError || !unicode.IsLower(r) {
		return s
	}
	word := s
	if i := strings.IndexFunc(s, unicode.IsSpace); i >= 0 {
		word = s[:i]
	}
	if strings.ContainsAny(word, "./:_`()[]") || strings.IndexFunc(word[size:], unicode.IsUpper) >= 0 {
		return s
	}
	return string(unicode.ToUpper(r)) + s[size:]
}

var (
	// "intended" counts only as a predicate ("this is intended", "as
	// intended"); "the intended caller" describes code, not a decision.
	intentRe = regexp.MustCompile(`(?i)\b(?:intentional(?:ly)?|(?:is|was|are|were|(?:it|this|that)(?:'|’)s) intended|intended behaviou?r|as intended|by design|on purpose|deliberate(?:ly)?|product (?:decision|call)|design (?:decision|choice)|not a bug|working as (?:intended|designed|expected)|(?:keep(?:ing)?|leav(?:e|ing)) (?:it|this|that|them|these)?\s*as[- ]is)\b`)
	// A negated intent phrase ("not intentional", "wasn't by design", "I
	// don't think this is on purpose") is a concession, and a promised fix
	// is a change even before it lands.
	negatedIntentRe = regexp.MustCompile(`(?i)\b(?:not|no|never|isn(?:'|’)?t|wasn(?:'|’)?t|aren(?:'|’)?t|weren(?:'|’)?t|don(?:'|’)?t|didn(?:'|’)?t|doesn(?:'|’)?t|unsure)\b(?:\s+\w+){0,3}\s+(?:intentional|intended|by design|on purpose|deliberate|working as|a (?:product|design) (?:decision|call|choice))`)
	// A change claim is affirmative fix language, not any change word:
	// "nothing changed" and "was added intentionally" are intent, not fixes,
	// and "this commit" only counts when it is said to change something.
	changeRe = regexp.MustCompile(`(?i)(?:\bfixed\b|\b(?:i|we)(?:(?:'|’)ve| have|(?:'|’)ll| will)? (?:fix|fixed|change|changed|moved|updated|removed|replaced|added|patched|pushed|split|lifted)\b|\bwill fix\b|\b(?:latest|new|this|that) commit (?:fixes|changes|moves|removes|adds|clears|drops|updates|replaces|addresses|handles|covers|guards)\b|\b(?:fixed|changed|addressed|handled) in (?:the )?(?:latest|new|this|that) commit\b|\bpushed\b|\bno longer\b|\bnow (?:works|returns|checks|guards|handles|rejects|clears|drops|skips|uses)\b|(?:\bin |\bat |\(|\bcommit )[0-9a-f]{7,40}\b)`)
	// "will not be fixed" and "we changed nothing" are the opposite of a fix.
	negatedChangeRe = regexp.MustCompile(`(?i)\b(?:(?:not|never|won(?:'|’)?t)\s+(?:be\s+|going\s+to\s+(?:be\s+)?)?(?:fix(?:ed|ing)?|chang(?:ed|ing)|updated?|moved?|removed?)\b|(?:fixed|changed|moved|updated|removed) nothing\b)`)
	deferRe         = regexp.MustCompile(`(?i)\b(?:out of scope|follow[- ]?up|later (?:pr|change|commit)|(?:separate|another|different|future|new|its own) (?:pr|ticket|change|issue)|not (?:touching|addressing|fixing|changing|doing) (?:it|this|that) here|(?:in|as) a ticket|track(?:s|ed|ing)? (?:it |this |that )?(?:separately|elsewhere))\b`)
	// "not a separate issue" and "no follow-up needed" are the opposite; so
	// are the verb "follow up with", a "follow-up commit" already pushed, and
	// a deferral that ends in "fixed it here".
	negatedDeferRe = regexp.MustCompile(`(?i)\b(?:(?:not|no|never|isn(?:'|’)?t|doesn(?:'|’)?t|don(?:'|’)?t|without)\b(?:\s+\w+){0,2}\s+(?:out of scope|follow[- ]?up|(?:separate|another|different|future|new) (?:pr|ticket|change|issue)|track(?:s|ed|ing)? (?:it |this |that )?(?:separately|elsewhere))|follow up (?:with|on|about)\b|follow[- ]?up (?:commit|push)\b|(?:fixed|done|addressed|handled|changed) (?:it |this |that )?(?:here|in this (?:pr|commit|branch))\b)`)
	// A key counts when the author talks about it as a ticket (or links it),
	// or leads a sentence with it ("PROJ-42 is the follow-up."); a bare GPT-4
	// or COVID-19 in passing is not one.
	ticketRe = regexp.MustCompile(`(?i)(?:\b(?:ticket|issue|jira|story|epic|track(?:s|ed|ing)?|filed|under|against|see|per|closes?|fixes|browse/|follow[- ]?up|scope|separate)\b:?[^.!?\n]{0,40}?|^\W*|[.!?;:]\s+)\b([A-Z][A-Z0-9]{1,9}-\d{1,6})\b`)
	// Uppercase-dash-digits that are not issue keys.
	notTicket  = map[string]bool{"SHA": true, "UTF": true, "ISO": true, "RFC": true, "MD": true, "AES": true, "HTTP": true, "TLS": true, "CVE": true, "CWE": true, "PR": true, "UTC": true, "RSA": true, "ES": true, "HTML": true, "GPT": true, "COVID": true, "SOC": true, "WCAG": true, "ARM": true, "OWASP": true, "PCI": true, "OAUTH": true, "X": true, "IPV": true}
	severityRe = regexp.MustCompile(`(?i)(?:\[(critical|high|medium|low)\]|alt="(critical|high|medium|low)")`)
	// The finding-withdrawal clause only. A bare "withdraw this/it" counts
	// when it opens the sentence or a clause ("Withdrawing this, since",
	// "..., so withdrawing this") or has a first-person subject ("I'll
	// withdraw it"); "the finding"/"this comment" as the object counts
	// anywhere. A domain "customers can withdraw it" stays. The "as ..." tail
	// is a closed list so it cannot eat the sentence that follows.
	withdrawRe = regexp.MustCompile(`(?i)(?:(?:^\W*|[,;]\s*)(?:(?:so|and|hence|therefore)\s+)?(?:` + firstPerson + `\s+)?|\b` + firstPerson + `\s+)withdraw(?:ing|n)?\s+(?:(?:the|this|my)\s+(?:finding|comment)|this|it)\b` + withdrawTail + `|\bwithdraw(?:ing|n)?\s+(?:the|this|my)\s+(?:finding|comment)\b` + withdrawTail + `|[,;]?\s*(?:so\s+)?(?:the\s+)?finding\s+(?:is\s+|stands\s+)?withdrawn\b|^\W*withdraw(?:n|ing)\W*$`)
	evidenceRe = regexp.MustCompile(`[\w./-]+:\d+|\bline \d+`)
	// A sentence ends at terminal punctuation followed by whitespace, so the
	// dots in retry.go:41 do not split it; a wrapped line is one sentence.
	sentenceRe = regexp.MustCompile(`(?s).*?[.!?]+["')\]]*(?:\s+|$)|.+$`)
	trackingRe = regexp.MustCompile(`\sTracking this against [A-Z][A-Z0-9]{1,9}-\d{1,6}\.$`)
	// The idempotency guards match an actual ask, not the words in passing:
	// "the issue key on cache.go:12" and "the description column" are evidence.
	ticketAskRe = regexp.MustCompile(`(?i)\bif there is a ticket\b|(?:\b(?:ticket|issue|jira)\b[^.!?]{0,40}\bkey\b|\bkey\b[^.!?]{0,40}\b(?:ticket|issue|jira)\b)[^.!?]*\?`)
	descAskRe   = regexp.MustCompile(`(?i)\baccepted risk\b|\bpr description\b`)
)

const (
	firstPerson  = `(?:i|we)(?:(?:'|’)(?:ll|d|m|re|ve)|\s+(?:will|would|am|are|have|can|should))?`
	withdrawTail = `(?:\s+as\s+(?:intended(?:\s+behaviou?r)?|(?:a\s+)?(?:product|design)\s+(?:decision|call|choice)|by\s+design|your\s+call|not\s+a\s+bug))?`
)

// AssertsIntent reports an author reply that defends the behavior as
// deliberate without describing a code change.
func AssertsIntent(comment string) bool {
	changed := changeRe.MatchString(comment) && !negatedChangeRe.MatchString(comment)
	return intentRe.MatchString(comment) && !negatedIntentRe.MatchString(comment) && !changed
}

// Defers reports an author reply that sends the fix elsewhere: out of scope,
// a follow-up, a later PR, a separate ticket.
func Defers(comment string) bool {
	return deferRe.MatchString(comment) && !negatedDeferRe.MatchString(comment)
}

// TicketKeys returns the issue keys (ABC-123) named in the text, in order,
// without duplicates.
func TicketKeys(text string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range ticketRe.FindAllStringSubmatch(text, -1) {
		key := strings.ToUpper(m[1])
		project := key[:strings.IndexByte(key, '-')]
		if notTicket[project] || seen[key] || strings.ToUpper(m[1]) != m[1] {
			continue
		}
		seen[key] = true
		out = append(out, key)
	}
	return out
}

// FindingSeverity reads the severity label PRism put at the head of the
// inline comment: "**[MEDIUM] ...**" or a badge image. Empty when absent.
func FindingSeverity(findingBody string) string {
	m := severityRe.FindStringSubmatch(findingBody)
	if m == nil {
		return ""
	}
	return strings.ToLower(m[1] + m[2])
}

// SeverityAtLeastMedium is the bar for naming a kept behavior as accepted risk.
func SeverityAtLeastMedium(severity string) bool {
	switch strings.ToLower(severity) {
	case "critical", "high", "medium":
		return true
	}
	return false
}

// Note names the sub-path a concession took, for telemetry; empty otherwise.
func Note(ctx Context) string {
	if ctx.Decision == "concede" && AssertsIntent(ctx.AuthorComment) {
		return NoteIntentAcknowledged
	}
	return ""
}

// Render applies the posting conventions to a reply body: no agreement
// formula opener; no withdrawal when the author asserted intent (and, at
// medium severity or above, the accepted-risk question); the ticket key
// repeated or asked for when the author deferred the fix. An answer to the
// author's question is left as written: a leading "Yes" is the answer there.
// ok is false when nothing postable remains. Rendering an already rendered
// body is a no-op.
func Render(body string, ctx Context) (out string, ok bool) {
	paragraph, appendix, ok := RenderParts(body, ctx)
	return paragraph + appendix, ok
}

// RenderParts is Render with the model's paragraph and the sentences the
// renderer appended returned separately, so a length cap can apply to the
// paragraph alone.
func RenderParts(body string, ctx Context) (paragraph, appendix string, ok bool) {
	if ctx.Decision == "answer" {
		paragraph = strings.TrimSpace(body)
		return paragraph, "", paragraph != ""
	}
	paragraph, ok = StripAgreementOpener(body)
	if !ok {
		return "", "", false
	}
	if Note(ctx) == NoteIntentAcknowledged {
		paragraph, ok = dropWithdrawal(paragraph)
		if !ok {
			return "", "", false
		}
		// Removing a leading withdrawal sentence can expose an opener.
		if paragraph, ok = StripAgreementOpener(paragraph); !ok {
			return "", "", false
		}
		if SeverityAtLeastMedium(FindingSeverity(ctx.FindingBody)) && !descAskRe.MatchString(paragraph) {
			appendix += " " + riskAskBare
		}
	}
	if Defers(ctx.AuthorComment) {
		keys := TicketKeys(ctx.AuthorComment)
		switch {
		case len(keys) > 0 && !containsAny(paragraph, keys):
			appendix += " Tracking this against " + keys[0] + "."
		case len(keys) == 0 && len(TicketKeys(paragraph)) == 0 && !ticketAskRe.MatchString(paragraph):
			appendix += " " + TicketAsk
		}
	}
	return paragraph, appendix, true
}

func containsAny(s string, subs []string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// AppendixLen is the rune count of the renderer's sentences at the end of a
// body rendered by an earlier step, when the step that appended them is not
// around to say. It reads the text, so a model that wrote the exact ticket
// ask itself is credited for it too; each sentence counts at most once.
func AppendixLen(body string) int {
	n := 0
	if m := trackingRe.FindString(body); m != "" {
		body = strings.TrimSuffix(body, m)
		n += len([]rune(m))
	} else if strings.HasSuffix(body, " "+TicketAsk) {
		body = strings.TrimSuffix(body, " "+TicketAsk)
		n += len([]rune(" " + TicketAsk))
	}
	if strings.HasSuffix(body, " "+riskAskBare) {
		n += len([]rune(" " + riskAskBare))
	}
	return n
}

// dropWithdrawal removes the withdrawal clause and any sentence that was
// nothing else. What remains must still put a consequence on the record: a
// body with no file:line left is not postable on the intent path. Sentences
// the clause removal did not touch are kept as written.
func dropWithdrawal(body string) (string, bool) {
	var kept []string
	for _, s := range sentenceRe.FindAllString(body, -1) {
		s = strings.TrimSpace(s)
		cut := strings.TrimSpace(withdrawRe.ReplaceAllString(s, ""))
		if cut == s {
			kept = append(kept, s)
			continue
		}
		cut = strings.TrimSpace(strings.TrimLeft(cut, ",;: "))
		if strings.Trim(cut, ".,;:!? ") == "" {
			continue
		}
		cut = strings.TrimSpace(strings.TrimRight(cut, ",;: ")) + lastPunct(cut)
		kept = append(kept, capitalize(cut))
	}
	out := strings.Join(kept, " ")
	if len(kept) == 0 || !evidenceRe.MatchString(out) {
		return "", false
	}
	return out, true
}

// lastPunct is empty when the sentence still ends in terminal punctuation
// (a closing quote or bracket after it included), or a period when the
// clause removal took it away.
func lastPunct(s string) string {
	s = strings.TrimRight(s, `"')]`)
	if strings.HasSuffix(s, ".") || strings.HasSuffix(s, "!") || strings.HasSuffix(s, "?") {
		return ""
	}
	return "."
}
