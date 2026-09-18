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
// it stands alone: followed by punctuation, by "that", or by nothing. "Right
// now the guard..." is not a formula and is left alone, and so is "You're
// right about the guard": dropping the formula there leaves a fragment no
// rule can turn back into a sentence, so the prompt alone covers that form.
var agreementOpenerRe = regexp.MustCompile(`(?i)^(?:i (?:think |believe |guess )?(?:you(?:'|’)?re right|you are right|agree)|you(?:'|’)?re right|you are right|that(?:'|’)?s (?:right|correct|fair)|correct|agreed|agree|good point|fair point|good catch|nice catch|fair enough|fair|right|yes|yep|indeed|exactly|true)(?:\s*[,.:;!]+\s*|\s+that\s+|\s*$)(?:(?:and|but|so)\s+)?`)

// StripAgreementOpener drops agreement formulas from the head of the body and
// capitalizes what remains. ok is false when nothing but formulas was there.
func StripAgreementOpener(body string) (out string, ok bool) {
	out = strings.TrimSpace(body)
	for i := 0; i < 4; i++ {
		m := agreementOpenerRe.FindStringIndex(out)
		if m == nil {
			break
		}
		out = strings.TrimSpace(out[m[1]:])
	}
	if out == "" {
		return "", false
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
	// A negated intent phrase ("not intentional", "wasn't by design") is a
	// concession, and a promised fix is a change even before it lands.
	negatedIntentRe = regexp.MustCompile(`(?i)\b(?:not|no|never|isn(?:'|’)?t|wasn(?:'|’)?t|aren(?:'|’)?t|weren(?:'|’)?t)\b(?:\s+\w+){0,2}\s+(?:intentional|intended|by design|on purpose|deliberate|working as)`)
	// A change claim is affirmative fix language, not any change word:
	// "nothing changed" and "was added intentionally" are intent, not fixes.
	changeRe = regexp.MustCompile(`(?i)(?:\bfixed\b|\b(?:i|we)(?:'ve| have|'ll| will)? (?:fix|fixed|change|changed|moved|updated|removed|replaced|added|patched|pushed|split|lifted)\b|\bwill fix\b|\b(?:latest|new|this|that) commit\b|\bpushed\b|(?:\bin |\bat |\(|\bcommit )[0-9a-f]{7,40}\b)`)
	deferRe  = regexp.MustCompile(`(?i)\b(?:out of scope|follow[- ]?up|later (?:pr|change|commit)|(?:separate|another|different|future|new|its own) (?:pr|ticket|change|issue)|not (?:touching|addressing|fixing|changing|doing) (?:it|this|that) here|(?:in|as) a ticket|track(?:ed|ing)? (?:it |this |that )?(?:separately|elsewhere))\b`)
	// "not a separate issue" and "no follow-up needed" are the opposite.
	negatedDeferRe = regexp.MustCompile(`(?i)\b(?:not|no|never|isn(?:'|’)?t|doesn(?:'|’)?t|without)\b(?:\s+\w+){0,2}\s+(?:out of scope|follow[- ]?up|(?:separate|another|different|future|new) (?:pr|ticket|change|issue))`)
	// A key counts when the author talks about it as a ticket (or links it);
	// a bare GPT-4 or COVID-19 in passing is not one.
	ticketRe = regexp.MustCompile(`(?i)(?:\b(?:ticket|issue|jira|story|epic|track(?:ed|ing)?|filed|under|against|see|per|closes?|fixes|browse/|follow[- ]?up|scope|separate)\b:?[^.!?\n]{0,40}?)\b([A-Z][A-Z0-9]{1,9}-\d{1,6})\b`)
	// Uppercase-dash-digits that are not issue keys.
	notTicket  = map[string]bool{"SHA": true, "UTF": true, "ISO": true, "RFC": true, "MD": true, "AES": true, "HTTP": true, "TLS": true, "CVE": true, "PR": true, "UTC": true, "RSA": true, "ES": true, "HTML": true}
	severityRe = regexp.MustCompile(`(?i)(?:\[(critical|high|medium|low)\]|alt="(critical|high|medium|low)")`)
	// The finding-withdrawal clause only: the verb needs the finding as its
	// object ("so withdrawing this", "I'll withdraw the finding"), or the
	// sentence is nothing but "Withdrawn." A domain "withdraw funds" stays.
	withdrawRe = regexp.MustCompile(`(?i)[,;]?\s*(?:(?:so|and|hence|therefore)\s+)?(?:(?:i|we)(?:'|’)?(?:ll| will|d| would)?\s+)?withdraw(?:ing)?\s+(?:this|it|the finding|the comment)\b(?:\s+as\s+[^.!?]*)?|[,;]?\s*(?:so\s+)?(?:the\s+)?finding\s+(?:is\s+|stands\s+)?withdrawn\b|^\W*withdraw(?:n|ing)\W*$`)
	evidenceRe = regexp.MustCompile(`[\w./-]+:\d+|\bline \d+`)
	// A sentence ends at terminal punctuation followed by whitespace, so the
	// dots in retry.go:41 do not split it.
	sentenceRe = regexp.MustCompile(`.*?[.!?]+["')\]]*(?:\s+|$)|.+$`)
	trackingRe = regexp.MustCompile(`\sTracking this against [A-Z][A-Z0-9]{1,9}-\d{1,6}\.$`)
	// The idempotency guards match an actual ask, not the words in passing:
	// "the issue key on cache.go:12" and "the description column" are evidence.
	ticketAskRe = regexp.MustCompile(`(?i)\bif there is a ticket\b|(?:\b(?:ticket|issue|jira)\b[^.!?]{0,40}\bkey\b|\bkey\b[^.!?]{0,40}\b(?:ticket|issue|jira)\b)[^.!?]*\?`)
	descAskRe   = regexp.MustCompile(`(?i)\baccepted risk\b|\bpr description\b`)
)

// AssertsIntent reports an author reply that defends the behavior as
// deliberate without describing a code change.
func AssertsIntent(comment string) bool {
	return intentRe.MatchString(comment) && !negatedIntentRe.MatchString(comment) && !changeRe.MatchString(comment)
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
// repeated or asked for when the author deferred the fix. ok is false when
// nothing postable remains. Rendering an already rendered body is a no-op.
func Render(body string, ctx Context) (out string, ok bool) {
	out, ok = StripAgreementOpener(body)
	if !ok {
		return "", false
	}
	if Note(ctx) == NoteIntentAcknowledged {
		out, ok = dropWithdrawal(out)
		if !ok {
			return "", false
		}
		if SeverityAtLeastMedium(FindingSeverity(ctx.FindingBody)) && !descAskRe.MatchString(out) {
			out = out + " " + riskAskBare
		}
	}
	if Defers(ctx.AuthorComment) {
		keys := TicketKeys(ctx.AuthorComment)
		switch {
		case len(keys) > 0 && !strings.Contains(out, keys[0]):
			out = out + " Tracking this against " + keys[0] + "."
		case len(keys) == 0 && len(TicketKeys(out)) == 0 && !ticketAskRe.MatchString(out):
			out = out + " " + TicketAsk
		}
	}
	return out, true
}

// AppendixLen is the rune count of the sentences Render appended to the end
// of body, so a length cap can apply to the model's paragraph alone.
func AppendixLen(body string) int {
	n := 0
	for {
		switch {
		case strings.HasSuffix(body, " "+TicketAsk):
			body = strings.TrimSuffix(body, " "+TicketAsk)
			n += len([]rune(" " + TicketAsk))
		case trackingRe.MatchString(body):
			m := trackingRe.FindString(body)
			body = strings.TrimSuffix(body, m)
			n += len([]rune(m))
		case strings.HasSuffix(body, " "+riskAskBare):
			body = strings.TrimSuffix(body, " "+riskAskBare)
			n += len([]rune(" " + riskAskBare))
		default:
			return n
		}
	}
}

// dropWithdrawal removes the withdrawal clause and any sentence that was
// nothing else. What remains must still put a consequence on the record: a
// body with no file:line left is not postable on the intent path.
func dropWithdrawal(body string) (string, bool) {
	var kept []string
	for _, s := range sentenceRe.FindAllString(body, -1) {
		s = strings.TrimSpace(withdrawRe.ReplaceAllString(s, ""))
		if strings.Trim(s, ".,;:!? ") == "" {
			continue
		}
		s = strings.TrimSpace(strings.TrimRight(s, ",;: ")) + strings.TrimLeft(lastPunct(s), " ")
		kept = append(kept, capitalize(s))
	}
	out := strings.Join(kept, " ")
	if len(kept) == 0 || !evidenceRe.MatchString(out) {
		return "", false
	}
	return out, true
}

// lastPunct is the sentence's terminal punctuation, or a period when the
// clause removal took it away.
func lastPunct(s string) string {
	if strings.HasSuffix(s, ".") || strings.HasSuffix(s, "!") || strings.HasSuffix(s, "?") {
		return ""
	}
	return "."
}
